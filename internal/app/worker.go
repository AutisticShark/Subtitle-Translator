package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AutisticShark/Subtitle-Translator/internal/engine"
)

// cancelPollInterval is how often a running job re-reads its status, so a
// cancellation recorded by another process (or by hand) is honored.
var cancelPollInterval = time.Second

var (
	errJobCanceled  = errors.New("job canceled")
	errShuttingDown = errors.New("server shutting down")
)

// jobRunner is the in-process job queue: a fixed number of workers take job
// ids in submission order. Close cancels running jobs and finalizes queued
// ones, so no job is left looking active after a clean shutdown; startup
// recovery handles crashes.
type jobRunner struct {
	s       *Server
	ctx     context.Context
	stop    context.CancelCauseFunc
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []string
	closed  bool
	running map[string]context.CancelCauseFunc
	wg      sync.WaitGroup
}

func newJobRunner(s *Server, workers int) *jobRunner {
	ctx, stop := context.WithCancelCause(context.Background())
	runner := &jobRunner{s: s, ctx: ctx, stop: stop, running: map[string]context.CancelCauseFunc{}}
	runner.cond = sync.NewCond(&runner.mu)
	for range workers {
		runner.wg.Add(1)
		go runner.work()
	}
	return runner
}

func (q *jobRunner) submit(jobID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.queue = append(q.queue, jobID)
	q.cond.Signal()
}

// cancel wakes the worker running jobID in this process, if any.
func (q *jobRunner) cancel(jobID string) {
	q.mu.Lock()
	cancel := q.running[jobID]
	q.mu.Unlock()
	if cancel != nil {
		cancel(errJobCanceled)
	}
}

func (q *jobRunner) work() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		for len(q.queue) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		jobID := q.queue[0]
		q.queue = q.queue[1:]
		ctx, cancel := context.WithCancelCause(q.ctx)
		q.running[jobID] = cancel
		q.mu.Unlock()

		q.s.runJob(ctx, jobID)

		q.mu.Lock()
		delete(q.running, jobID)
		q.mu.Unlock()
		cancel(nil)
	}
}

func (q *jobRunner) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	pending := q.queue
	q.queue = nil
	q.cond.Broadcast()
	q.mu.Unlock()
	q.stop(errShuttingDown)
	q.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, jobID := range pending {
		q.s.finalizeInterrupted(ctx, jobID)
	}
}

// writeJob updates a job only while it is in one of statuses (rule 5) and
// bumps the job cache revision when it changed; it reports whether it did.
func (s *Server) writeJob(ctx context.Context, jobID string, statuses []string, fields map[string]any) (bool, error) {
	fields["updated_at"] = isoSeconds(s.now())
	names := sortedKeys(fields)
	assignments := make([]string, len(names))
	args := make([]any, 0, len(names)+len(statuses)+1)
	for i, name := range names {
		assignments[i] = name + " = ?"
		args = append(args, fields[name])
	}
	args = append(args, jobID)
	query := "UPDATE jobs SET " + strings.Join(assignments, ", ") + " WHERE id = ?"
	if statuses != nil {
		query += " AND status IN (" + placeholders(len(statuses)) + ")"
		args = append(args, stringArgs(statuses)...)
	}
	changed := false
	err := s.db.tx(ctx, func(c conn) error {
		count, err := c.exec(ctx, query, args...)
		if err != nil {
			return err
		}
		changed = count == 1
		if changed {
			return bumpCacheRevision(ctx, c, "jobs")
		}
		return nil
	})
	return changed, err
}

// finalizeInterrupted ends a job this process can no longer finish, exactly
// as startup recovery would.
func (s *Server) finalizeInterrupted(ctx context.Context, jobID string) {
	if _, err := s.writeJob(ctx, jobID, []string{"queued", "processing"}, map[string]any{
		"status": "failed", "stage": "Translation failed", "error": recoveredJobError,
	}); err != nil {
		s.logger.Error("could not finalize an interrupted job", "job", jobID, "error", err)
	}
	if _, err := s.writeJob(ctx, jobID, []string{"canceling"}, map[string]any{
		"status": "canceled", "stage": "Canceled", "error": nil,
	}); err != nil {
		s.logger.Error("could not finalize an interrupted job", "job", jobID, "error", err)
	}
}

func (s *Server) markCanceled(ctx context.Context, jobID string) {
	if _, err := s.writeJob(ctx, jobID, []string{"queued", "processing", "canceling"}, map[string]any{
		"status": "canceled", "stage": "Canceled", "error": nil,
	}); err != nil {
		s.logger.Error("could not record a job cancellation", "job", jobID, "error", err)
	}
}

// watchCancellation polls the job status while it runs and cancels ctx when
// the job left queued/processing (canceled by another process or by hand).
func (s *Server) watchCancellation(ctx context.Context, cancel context.CancelCauseFunc, jobID string) {
	ticker := time.NewTicker(cancelPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var status string
			err := s.db.conn().queryRow(ctx, "SELECT status FROM jobs WHERE id = ?", jobID).Scan(&status)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("Could not poll the cancellation state of a job", "job", jobID, "error", err)
				}
				continue
			}
			if status != "queued" && status != "processing" {
				cancel(errJobCanceled)
				return
			}
		}
	}
}

func optionString(options map[string]any, key string) string {
	switch value := options[key].(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func optionInt(options map[string]any, key string) (int, error) {
	text := pythonStrip(optionString(options, key))
	value, err := strconv.Atoi(text)
	if err != nil {
		if parsed, floatErr := strconv.ParseFloat(text, 64); floatErr == nil && parsed == math.Trunc(parsed) {
			return int(parsed), nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: %q", text)
	}
	return value, nil
}

func optionFloat(options map[string]any, key string) (float64, error) {
	value, err := parsePythonFloat(optionString(options, key))
	if err != nil {
		return 0, fmt.Errorf("could not convert string to float: %q", optionString(options, key))
	}
	return value, nil
}

// jobFailure is a stored "<Type>: <message>" error.
type jobFailure struct {
	kind, message string
}

func (e *jobFailure) Error() string { return e.kind + ": " + e.message }

func storedError(err error) string {
	var failure *jobFailure
	if errors.As(err, &failure) {
		return failure.Error()
	}
	return engine.ErrorTypeName(err) + ": " + err.Error()
}

// runJob translates one job (run_job). Every transition is conditional on the
// current status, so a concurrent cancellation always wins over completion,
// failure or progress.
func (s *Server) runJob(ctx context.Context, jobID string) {
	cancelCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	db := context.WithoutCancel(ctx)
	go s.watchCancellation(cancelCtx, cancel, jobID)

	err := s.translateJobRecovered(cancelCtx, db, jobID)
	cause := context.Cause(cancelCtx)
	switch {
	case err == nil:
		return
	case errors.Is(cause, errShuttingDown):
		s.finalizeInterrupted(db, jobID)
	case errors.Is(err, errJobCanceled) || errors.Is(cause, errJobCanceled):
		s.markCanceled(db, jobID)
	default:
		changed, writeErr := s.writeJob(db, jobID, []string{"queued", "processing"}, map[string]any{
			"status": "failed", "stage": "Translation failed", "error": storedError(err),
		})
		if writeErr != nil {
			s.logger.Error("could not record a job failure", "job", jobID, "error", writeErr)
		}
		if !changed {
			// A cancellation recorded concurrently wins over the failure; never
			// leave the job stuck in "canceling".
			s.markCanceled(db, jobID)
		}
	}
}

// translateJobRecovered turns a panic in the job (for example in the engine)
// into a recorded failure, as the Python worker recorded any exception; an
// unrecovered panic in a worker goroutine would stop the whole server.
func (s *Server) translateJobRecovered(ctx, db context.Context, jobID string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("translation job panicked", "job", jobID, "panic_type", fmt.Sprintf("%T", recovered),
				"stack", string(debug.Stack()))
			err = &jobFailure{"RuntimeError", "internal error while translating"}
		}
	}()
	return s.translateJob(ctx, db, jobID)
}

// translateJob returns nil when the job finished (completed, or already
// finalized elsewhere), errJobCanceled when it should end as canceled, or the
// failure to record.
func (s *Server) translateJob(ctx, db context.Context, jobID string) error {
	rows, err := queryJobs(db, s.db.conn(), false, "SELECT "+jobColumns+" FROM jobs WHERE jobs.id = ?", jobID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	row := rows[0]
	if row.Status == "canceling" || ctx.Err() != nil {
		return errJobCanceled
	}
	decoder := json.NewDecoder(strings.NewReader(row.Options))
	decoder.UseNumber()
	var options map[string]any
	if err := decoder.Decode(&options); err != nil {
		return &jobFailure{"JSONDecodeError", err.Error()}
	}
	folder, ok := s.jobFolder(jobID)
	if !ok {
		return &jobFailure{"ValueError", "Invalid job path"}
	}
	claimed, err := s.writeJob(db, jobID, []string{"queued"}, map[string]any{
		"status": "processing", "progress": 2, "stage": "Reading subtitle",
	})
	if err != nil || !claimed {
		// Already claimed, canceled while queued, or finalized elsewhere.
		return err
	}
	report := func(fields map[string]any) {
		// Never overwrite "Canceling", or a state written elsewhere, with
		// progress from work that is being abandoned.
		if _, err := s.writeJob(db, jobID, []string{"processing"}, fields); err != nil {
			s.logger.Warn("could not record job progress", "job", jobID, "error", err)
		}
	}
	source, err := os.ReadFile(filepath.Join(folder, row.StoredName))
	if err != nil {
		return &jobFailure{"FileNotFoundError", "the uploaded subtitle could not be read"}
	}
	extension := strings.ToLower(filepath.Ext(row.StoredName))
	encoding := optionString(options, "encoding")
	if _, present := options["encoding"]; !present {
		encoding = "utf-8"
	}
	cues, err := s.translator.CountCues(source, extension, encoding)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errJobCanceled
	}
	report(map[string]any{"progress": 5, "stage": fmt.Sprintf("Parsed %d cues", cues)})

	provider := optionString(options, "provider")
	config, err := s.engineConfig(db, provider, options)
	if err != nil {
		return err
	}
	var targets []string
	if list, ok := options["target_languages"].([]any); ok {
		for _, item := range list {
			if text, ok := item.(string); ok {
				targets = append(targets, text)
			}
		}
	}
	if len(targets) == 0 {
		return &jobFailure{"ValueError", "No target languages"}
	}
	// One throttle and cache per job, shared by its targets as in the Python worker.
	config.Throttle = engine.NewThrottle(config.RPM)
	config.Cache = engine.NewCache()
	outputs := []jobOutput{}
	untranslated, segments := 0, 0
	for index, language := range targets {
		if ctx.Err() != nil {
			return errJobCanceled
		}
		details, ok := s.languages[language]
		if !ok {
			return &jobFailure{"KeyError", strconv.Quote(language)}
		}
		count := float64(len(targets))
		report(map[string]any{
			"progress": 5 + roundHalfEven(float64(index)/count*90),
			"stage":    "Translating to " + details.Name,
		})
		progress := func(done, total int) {
			fraction := 1.0
			if total > 0 {
				fraction = float64(done) / float64(total)
			}
			report(map[string]any{
				"progress": 5 + roundHalfEven((float64(index)+fraction)/count*90),
				"stage":    fmt.Sprintf("Translating to %s (%d/%d segments)", details.Name, done, total),
			})
		}
		translated, stats, err := s.translator.Translate(ctx, source, extension, language, config, progress)
		if err != nil {
			if ctx.Err() != nil || engine.IsCanceled(err) {
				return errJobCanceled
			}
			return err
		}
		if ctx.Err() != nil {
			return errJobCanceled
		}
		untranslated += stats.Fallbacks
		segments += stats.Segments
		name := engine.TranslatedFilenameSuffix(row.Filename, details.Suffix)
		if err := os.WriteFile(filepath.Join(folder, name), translated, 0o644); err != nil {
			return &jobFailure{"OSError", "could not write the translated subtitle"}
		}
		outputs = append(outputs, jobOutput{Name: name, Language: language})
		encoded, _ := json.Marshal(outputs)
		report(map[string]any{"outputs": string(encoded), "progress": 5 + roundHalfEven(float64(index+1)/count*90)})
	}
	encoded, _ := json.Marshal(outputs)
	var warning any
	if untranslated > 0 {
		warning = fmt.Sprintf("Left untranslated: %d of %d segments", untranslated, segments)
	}
	completed, err := s.writeJob(db, jobID, []string{"processing"}, map[string]any{
		"status": "completed", "progress": 100, "stage": "Ready to download", "outputs": string(encoded),
		"error": nil, "warning": warning,
	})
	if err != nil {
		return err
	}
	if !completed {
		return errJobCanceled
	}
	return nil
}

// engineConfig selects the provider settings and decrypts only that
// provider's key (rule 27). Echo is accepted only in debug mode (rule 8).
func (s *Server) engineConfig(ctx context.Context, provider string, options map[string]any) (engine.Config, error) {
	fatal := func(message string) (engine.Config, error) {
		return engine.Config{}, &jobFailure{"FatalTranslationError", message}
	}
	settings, err := s.readSettings(ctx, provider+"_api_key")
	if err != nil {
		return engine.Config{}, &jobFailure{"RuntimeError", err.Error()}
	}
	config := engine.Config{Provider: provider}
	labels := map[string]string{
		"anthropic": "Anthropic", "openai": "OpenAI-compatible", "deepl": "DeepL", "google": "Google Cloud Translation",
	}
	switch provider {
	case "echo":
		if !s.cfg.Debug {
			return fatal("Echo provider is available only in debug mode")
		}
	case "anthropic", "openai", "deepl", "google":
		config.APIKey = settings.secrets[provider+"_api_key"]
		if config.APIKey == "" {
			return fatal(labels[provider] + " API key is not configured")
		}
	default:
		return fatal("Unknown provider: " + provider)
	}
	model := optionString(options, "model")
	switch provider {
	case "anthropic":
		if model == "" {
			model = settings.get("anthropic_model")
		}
		config.Model = model
	case "openai":
		if model == "" {
			model = settings.get("openai_model")
		}
		config.Model = model
		config.BaseURL = settings.get("openai_base_url")
	}
	config.SourceLang = optionString(options, "source_language")
	config.Encoding = optionString(options, "encoding")
	if _, present := options["encoding"]; !present {
		config.Encoding = "utf-8"
	}
	if config.BatchSize, err = optionInt(options, "batch_size"); err != nil {
		return engine.Config{}, &jobFailure{"ValueError", err.Error()}
	}
	if config.Workers, err = optionInt(options, "workers"); err != nil {
		return engine.Config{}, &jobFailure{"ValueError", err.Error()}
	}
	if config.MaxLines, err = optionInt(options, "max_lines"); err != nil {
		return engine.Config{}, &jobFailure{"ValueError", err.Error()}
	}
	if config.Width, err = optionFloat(options, "width"); err != nil {
		return engine.Config{}, &jobFailure{"ValueError", err.Error()}
	}
	if config.RPM, err = optionFloat(options, "rpm"); err != nil {
		return engine.Config{}, &jobFailure{"ValueError", err.Error()}
	}
	return config, nil
}
