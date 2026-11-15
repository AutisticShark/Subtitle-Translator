package app

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	terminalStatuses = map[string]bool{"completed": true, "failed": true, "canceled": true}
	// submitterVisibleErrors are failures with the submitter's own file. Every
	// other stored error may quote a provider response and is admin-only.
	submitterVisibleErrors = map[string]bool{
		"SubtitleFormatError": true, "UnicodeDecodeError": true, "UnicodeError": true, "LookupError": true,
	}
	knownStages = map[string]bool{
		"Reading subtitle": true, "Ready to download": true, "Canceled": true, "Canceling": true,
		"Translation failed": true,
	}
	parsedStage      = regexp.MustCompile(`^Parsed (\d+) cues$`)
	translatingStage = regexp.MustCompile(`^Translating to (.+?)(?: \((\d+)/(\d+) segments\))?$`)
	untranslatedNote = regexp.MustCompile(`^Left untranslated: (\d+) of (\d+) segments$`)
)

// jobRow is a jobs row (plus the owner's username in admin listings). It is
// cached raw and locale-neutral; jobDict localizes and redacts per viewer.
type jobRow struct {
	ID         string  `json:"id"`
	UserID     *string `json:"user_id"`
	Filename   string  `json:"filename"`
	StoredName string  `json:"stored_name"`
	Status     string  `json:"status"`
	Progress   int64   `json:"progress"`
	Stage      string  `json:"stage"`
	Options    string  `json:"options"`
	Outputs    string  `json:"outputs"`
	Error      *string `json:"error"`
	Warning    *string `json:"warning"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	Owner      *string `json:"owner,omitempty"`
}

const jobColumns = "jobs.id, jobs.user_id, jobs.filename, jobs.stored_name, jobs.status, jobs.progress, " +
	"jobs.stage, jobs.options, jobs.outputs, jobs.error, jobs.warning, jobs.created_at, jobs.updated_at"

func scanJob(scanner interface{ Scan(...any) error }, withOwner bool) (*jobRow, error) {
	var row jobRow
	var userID, errText, warning, owner, stage, outputs sql.NullString
	targets := []any{&row.ID, &userID, &row.Filename, &row.StoredName, &row.Status, &row.Progress, &stage,
		&row.Options, &outputs, &errText, &warning, &row.CreatedAt, &row.UpdatedAt}
	if withOwner {
		targets = append(targets, &owner)
	}
	if err := scanner.Scan(targets...); err != nil {
		return nil, err
	}
	row.Stage = stage.String
	row.Outputs = outputs.String
	if row.Outputs == "" {
		row.Outputs = "[]"
	}
	if userID.Valid {
		row.UserID = &userID.String
	}
	if errText.Valid {
		row.Error = &errText.String
	}
	if warning.Valid {
		row.Warning = &warning.String
	}
	if withOwner && owner.Valid {
		row.Owner = &owner.String
	}
	return &row, nil
}

func queryJobs(ctx context.Context, c conn, withOwner bool, query string, args ...any) ([]jobRow, error) {
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []jobRow{}
	for rows.Next() {
		row, err := scanJob(rows, withOwner)
		if err != nil {
			return nil, err
		}
		result = append(result, *row)
	}
	return result, rows.Err()
}

type jobOutput struct {
	Name     string `json:"name"`
	Language string `json:"language"`
}

func (row *jobRow) outputs() []jobOutput {
	var outputs []jobOutput
	if err := json.Unmarshal([]byte(row.Outputs), &outputs); err != nil {
		return nil
	}
	return outputs
}

// jobScope is the WHERE clause for one job visible to user: administrators see
// every owner's jobs, everyone else only their own (others read as not found).
func jobScope(jobID string, user *userRow) (string, []any) {
	if user.Role == "admin" {
		return "jobs.id = ?", []any{jobID}
	}
	return "jobs.id = ? AND jobs.user_id = ?", []any{jobID, user.ID}
}

func (s *Server) localizedStage(locale, stage string) string {
	if knownStages[stage] {
		return s.tr(locale, stage)
	}
	if match := parsedStage.FindStringSubmatch(stage); match != nil {
		return s.tr(locale, "Parsed {count} cues", "count", match[1])
	}
	if match := translatingStage.FindStringSubmatch(stage); match != nil {
		language := s.tr(locale, match[1])
		if match[2] != "" {
			return s.tr(locale, "Translating to {language} ({done}/{total} segments)",
				"language", language, "done", match[2], "total", match[3])
		}
		return s.tr(locale, "Translating to {language}", "language", language)
	}
	return stage
}

func (s *Server) localizedWarning(locale string, warning *string) any {
	if warning == nil || *warning == "" {
		return nil
	}
	if match := untranslatedNote.FindStringSubmatch(*warning); match != nil {
		return s.tr(locale, "Left untranslated: {count} of {total} segments", "count", match[1], "total", match[2])
	}
	return *warning
}

func (s *Server) visibleJobError(locale string, stored *string, viewer *userRow) any {
	if stored == nil || *stored == "" {
		if stored == nil {
			return nil
		}
		return *stored
	}
	if viewer.Role == "admin" {
		return *stored
	}
	kind, _, _ := strings.Cut(*stored, ":")
	if submitterVisibleErrors[kind] {
		return *stored
	}
	return s.tr(locale, "Translation failed. An administrator can see the details in the translation history.")
}

// jobDict serializes a raw row for viewer: localized, and redacted for non-admins.
func (s *Server) jobDict(locale string, row *jobRow, viewer *userRow, includeOwner bool) map[string]any {
	var options any = json.RawMessage("null")
	if json.Valid([]byte(row.Options)) {
		options = json.RawMessage(row.Options)
	}
	var outputs any = []any{}
	if json.Valid([]byte(row.Outputs)) {
		outputs = json.RawMessage(row.Outputs)
	}
	result := map[string]any{
		"id": row.ID, "filename": row.Filename, "status": row.Status, "progress": row.Progress,
		"stage": s.localizedStage(locale, row.Stage), "options": options, "outputs": outputs,
		"error": s.visibleJobError(locale, row.Error, viewer), "warning": s.localizedWarning(locale, row.Warning),
		"created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
	}
	if includeOwner {
		result["owner"] = row.Owner
	}
	return result
}

// jobOptions is the options column of a new job.
type jobOptions struct {
	Provider        string   `json:"provider"`
	Model           *string  `json:"model"`
	SourceLanguage  string   `json:"source_language"`
	TargetLanguages []string `json:"target_languages"`
	Encoding        string   `json:"encoding"`
	BatchSize       string   `json:"batch_size"`
	Workers         string   `json:"workers"`
	RPM             string   `json:"rpm"`
	Width           string   `json:"width"`
	MaxLines        string   `json:"max_lines"`
}

func formValue(form *multipart.Form, values map[string][]string, key, fallback string) string {
	if form != nil {
		if list, ok := form.Value[key]; ok && len(list) > 0 {
			return list[0]
		}
	}
	if list, ok := values[key]; ok && len(list) > 0 {
		return list[0]
	}
	return fallback
}

type stagedUpload struct {
	jobID, folder, displayName, stored string
}

func (s *Server) handleCreateJobs(w http.ResponseWriter, r *http.Request, auth *authResult) {
	ctx := r.Context()
	tooLarge := func() {
		s.jsonError(w, r, http.StatusRequestEntityTooLarge, "Upload exceeds the {max_upload_mb} MB limit",
			"max_upload_mb", s.cfg.MaxUploadMB)
	}
	var values map[string][]string
	err := r.ParseMultipartForm(32 << 20)
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr):
		tooLarge()
		return
	case errors.Is(err, http.ErrNotMultipart):
		if err := r.ParseForm(); err != nil {
			if errors.As(err, &maxErr) {
				tooLarge()
				return
			}
		}
		values = r.PostForm
	case err != nil:
		if strings.Contains(err.Error(), "too large") {
			tooLarge()
			return
		}
		s.jsonError(w, r, http.StatusBadRequest, "Select at least one subtitle file")
		return
	}
	form := r.MultipartForm
	if form != nil {
		defer form.RemoveAll()
	}
	get := func(key, fallback string) string { return formValue(form, values, key, fallback) }
	failure, err := s.verifyCaptcha(ctx, r, "upload", get("captcha_token", ""))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"error": failure.message})
		return
	}
	var files []*multipart.FileHeader
	if form != nil {
		files = append(append(files, form.File["files"]...), form.File["files[]"]...)
	}
	anyNamed := false
	for _, file := range files {
		if file.Filename != "" {
			anyNamed = true
		}
	}
	if !anyNamed {
		s.jsonError(w, r, http.StatusBadRequest, "Select at least one subtitle file")
		return
	}
	displayNames := make([]string, len(files))
	for i, file := range files {
		if displayNames[i] = uploadDisplayName(file.Filename); displayNames[i] == "" {
			s.jsonError(w, r, http.StatusBadRequest, "Unsupported file: {filename}", "filename", file.Filename)
			return
		}
	}
	settings, err := s.readSettings(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	provider := get("provider", settings.get("default_provider"))
	if !contains(s.availableProviders(), provider) {
		s.jsonError(w, r, http.StatusBadRequest, "Invalid provider")
		return
	}
	// Duplicates would translate the same language repeatedly into one output name.
	var targets []string
	for _, value := range strings.Split(get("target_languages", settings.get("target_languages")), ",") {
		if value = pythonStrip(value); value != "" && !contains(targets, value) {
			targets = append(targets, value)
		}
	}
	valid := len(targets) > 0
	for _, language := range targets {
		if _, ok := s.languages[language]; !ok {
			valid = false
		}
	}
	if !valid {
		s.jsonError(w, r, http.StatusBadRequest, "Choose one or more valid target languages")
		return
	}
	if len(targets) > maxTargetLanguages {
		s.jsonError(w, r, http.StatusBadRequest, "Choose at most {maximum} target languages",
			"maximum", maxTargetLanguages)
		return
	}
	var model *string
	if text := pythonStrip(get("model", "")); text != "" {
		model = &text
	}
	optionsJSON, err := json.Marshal(jobOptions{
		Provider: provider, Model: model,
		SourceLanguage:  pythonStrip(get("source_language", settings.get("source_language"))),
		TargetLanguages: targets, Encoding: pythonStrip(get("encoding", "utf-8")),
		BatchSize: settings.get("batch_size"), Workers: settings.get("workers"), RPM: settings.get("rpm"),
		Width: settings.get("width"), MaxLines: settings.get("max_lines"),
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var staged []stagedUpload
	cleanup := func() {
		for _, item := range staged {
			_ = os.RemoveAll(item.folder)
		}
	}
	for i, file := range files {
		item := stagedUpload{jobID: uuidHex(), displayName: displayNames[i],
			stored: "source" + strings.ToLower(filepath.Ext(displayNames[i]))}
		item.folder = filepath.Join(s.jobsDir, item.jobID)
		if err := os.Mkdir(item.folder, 0o755); err != nil {
			cleanup()
			s.internalError(w, r, err)
			return
		}
		staged = append(staged, item)
		if err := saveUpload(file, filepath.Join(item.folder, item.stored)); err != nil {
			cleanup()
			s.internalError(w, r, err)
			return
		}
	}
	var exceeded map[string]any
	err = s.db.tx(ctx, func(c conn) error {
		exceeded = nil
		var err error
		if exceeded, err = s.consumeJobQuota(ctx, c, localeOf(r), auth.user, len(staged)); err != nil || exceeded != nil {
			return err
		}
		timestamp := isoSeconds(s.now())
		for _, item := range staged {
			if _, err := c.exec(ctx, "INSERT INTO jobs (id, user_id, filename, stored_name, status, progress, stage, "+
				"options, outputs, error, warning, created_at, updated_at) VALUES (?, ?, ?, ?, 'queued', 0, '', ?, '[]', "+
				"NULL, NULL, ?, ?)", item.jobID, auth.user.ID, item.displayName, item.stored, string(optionsJSON),
				timestamp, timestamp); err != nil {
				return err
			}
		}
		return bumpCacheRevision(ctx, c, "jobs")
	})
	if err != nil {
		cleanup()
		s.internalError(w, r, err)
		return
	}
	if exceeded != nil {
		cleanup()
		w.Header().Set("Retry-After", fmt.Sprint(exceeded["retry_after"]))
		writeJSON(w, http.StatusTooManyRequests, exceeded)
		return
	}
	created := make([]string, len(staged))
	for i, item := range staged {
		created[i] = item.jobID
		s.runner.submit(item.jobID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"jobs": created})
}

func saveUpload(header *multipart.FileHeader, destination string) error {
	source, err := header.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		target.Close()
		return err
	}
	return target.Close()
}

// boundedIntArg reads an integer query argument, falling back on garbage and
// clamping to the bounds.
func boundedIntArg(r *http.Request, name string, fallback, minimum, maximum int) int {
	value := fallback
	if raw, ok := r.URL.Query()[name]; ok && len(raw) > 0 {
		text := strings.ReplaceAll(pythonStrip(raw[0]), "_", "")
		parsed, err := strconv.ParseInt(text, 10, 64)
		var numErr *strconv.NumError
		switch {
		case err == nil:
			value = int(max(min(parsed, int64(maximum)), int64(minimum)))
		case errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange):
			if strings.HasPrefix(text, "-") {
				value = minimum
			} else {
				value = maximum
			}
		}
	}
	return min(max(value, minimum), maximum)
}

type statusCount struct {
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request, auth *authResult) {
	ctx := r.Context()
	user := auth.user
	limit := boundedIntArg(r, "limit", 50, 1, 200)
	offset := boundedIntArg(r, "offset", 0, 0, jobListMaxOffset)
	showAll := user.Role == "admin" && r.URL.Query().Get("all") == "1"
	scope := user.ID
	where, args := " WHERE jobs.user_id = ?", []any{user.ID}
	if showAll {
		scope, where, args = "all", "", nil
	}
	// The id tie-breaker keeps pages stable for jobs from one multi-file upload.
	rows, err := cachedQuery(ctx, s, "jobs", fmt.Sprintf("list:%s:%d:%d", scope, limit, offset),
		func(c conn) ([]jobRow, error) {
			return queryJobs(ctx, c, true, "SELECT "+jobColumns+", users.username FROM jobs LEFT OUTER JOIN users "+
				"ON jobs.user_id = users.id"+where+" ORDER BY jobs.created_at DESC, jobs.id DESC LIMIT "+
				strconv.Itoa(limit)+" OFFSET "+strconv.Itoa(offset), args...)
		})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	counts, err := cachedQuery(ctx, s, "jobs", "counts:"+scope, func(c conn) ([]statusCount, error) {
		result, err := c.query(ctx, "SELECT jobs.status, COUNT(*) FROM jobs"+where+" GROUP BY jobs.status", args...)
		if err != nil {
			return nil, err
		}
		defer result.Close()
		list := []statusCount{}
		for result.Next() {
			var item statusCount
			if err := result.Scan(&item.Status, &item.Count); err != nil {
				return nil, err
			}
			list = append(list, item)
		}
		return list, result.Err()
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	countMap := map[string]int64{}
	var total int64
	for _, item := range counts {
		countMap[item.Status] = item.Count
		total += item.Count
	}
	locale := localeOf(r)
	jobs := make([]map[string]any, len(rows))
	for i := range rows {
		jobs[i] = s.jobDict(locale, &rows[i], user, showAll)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs, "total": total, "counts": countMap, "offset": offset, "limit": limit,
		"has_more": int64(offset+len(rows)) < total,
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request, auth *authResult) {
	ctx := r.Context()
	jobID := r.PathValue("job_id")
	user := auth.user
	where, args := jobScope(jobID, user)
	rows, err := cachedQuery(ctx, s, "jobs", fmt.Sprintf("detail:%s:%s:%s", user.Role, user.ID, jobID),
		func(c conn) ([]jobRow, error) {
			return queryJobs(ctx, c, false, "SELECT "+jobColumns+" FROM jobs WHERE "+where, args...)
		})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(rows) == 0 {
		s.jsonError(w, r, http.StatusNotFound, "Job not found")
		return
	}
	writeJSON(w, http.StatusOK, s.jobDict(localeOf(r), &rows[0], user, false))
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request, auth *authResult) {
	ctx := r.Context()
	jobID := r.PathValue("job_id")
	where, args := jobScope(jobID, auth.user)
	var row *jobRow
	var changed int64
	err := s.db.tx(ctx, func(c conn) error {
		row, changed = nil, 0
		count, err := c.scalarInt(ctx, "SELECT COUNT(*) FROM jobs WHERE "+where, args...)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		timestamp := isoSeconds(s.now())
		// Conditional transitions: a queued job has no worker and becomes
		// terminal at once; a running job must be acknowledged by its worker.
		changed, err = c.exec(ctx, "UPDATE jobs SET status = 'canceled', stage = 'Canceled', error = NULL, "+
			"updated_at = ? WHERE "+where+" AND jobs.status = 'queued'", append([]any{timestamp}, args...)...)
		if err != nil {
			return err
		}
		if changed == 0 {
			changed, err = c.exec(ctx, "UPDATE jobs SET status = 'canceling', stage = 'Canceling', updated_at = ? "+
				"WHERE "+where+" AND jobs.status = 'processing'", append([]any{timestamp}, args...)...)
			if err != nil {
				return err
			}
		}
		if changed > 0 {
			if err := bumpCacheRevision(ctx, c, "jobs"); err != nil {
				return err
			}
		}
		rows, err := queryJobs(ctx, c, false, "SELECT "+jobColumns+" FROM jobs WHERE "+where, args...)
		if err != nil || len(rows) == 0 {
			return err
		}
		row = &rows[0]
		return nil
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if row == nil {
		s.jsonError(w, r, http.StatusNotFound, "Job not found")
		return
	}
	if row.Status == "canceling" {
		s.runner.cancel(row.ID)
		writeJSON(w, http.StatusAccepted, s.jobDict(localeOf(r), row, auth.user, false))
		return
	}
	if changed > 0 && row.Status == "canceled" {
		writeJSON(w, http.StatusAccepted, s.jobDict(localeOf(r), row, auth.user, false))
		return
	}
	s.jsonError(w, r, http.StatusConflict, "Only an active job can be canceled")
}

// jobFolder resolves a job's folder, refusing anything outside the jobs directory.
func (s *Server) jobFolder(jobID string) (string, bool) {
	folder := filepath.Join(s.jobsDir, jobID)
	if jobID == "" || filepath.Dir(folder) != filepath.Clean(s.jobsDir) || filepath.Base(folder) != jobID {
		return "", false
	}
	return folder, true
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request, auth *authResult) {
	ctx := r.Context()
	jobID := r.PathValue("job_id")
	where, args := jobScope(jobID, auth.user)
	var deleted string
	err := s.db.tx(ctx, func(c conn) error {
		// The stored id names the folder: MySQL/MariaDB compare ids
		// case-insensitively, so the path's spelling may differ from it.
		var rowID, status string
		deleted = ""
		err := c.queryRow(ctx, "SELECT jobs.id, jobs.status FROM jobs WHERE "+where, args...).Scan(&rowID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return &txResponse{http.StatusNotFound, "Job not found"}
		}
		if err != nil {
			return err
		}
		if !terminalStatuses[status] {
			return &txResponse{http.StatusConflict, "Wait for the job to finish before deleting it"}
		}
		folder, ok := s.jobFolder(rowID)
		if !ok {
			return &txResponse{http.StatusBadRequest, "Invalid job path"}
		}
		// Files first: a failed row delete leaves a self-healing dangling row,
		// while the reverse order would orphan folders.
		if err := os.RemoveAll(folder); err != nil {
			s.logger.Error("could not delete job files", "job", rowID, "error", err)
			return &txResponse{http.StatusInternalServerError, "Could not delete the job files"}
		}
		if _, err := c.exec(ctx, "DELETE FROM jobs WHERE id = ?", rowID); err != nil {
			return err
		}
		deleted = rowID
		return bumpCacheRevision(ctx, c, "jobs")
	})
	if s.writeTxError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) ownedJob(ctx context.Context, jobID string, user *userRow) (*jobRow, error) {
	where, args := jobScope(jobID, user)
	rows, err := queryJobs(ctx, s.db.conn(), false, "SELECT "+jobColumns+" FROM jobs WHERE "+where, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func outputContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt":
		return "application/x-subrip"
	case ".vtt":
		return "text/vtt; charset=utf-8"
	}
	return "application/octet-stream"
}

func (s *Server) handleDownloadOutput(w http.ResponseWriter, r *http.Request, auth *authResult) {
	jobID := r.PathValue("job_id")
	row, err := s.ownedJob(r.Context(), jobID, auth.user)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if row == nil {
		s.jsonError(w, r, http.StatusNotFound, "Job not found")
		return
	}
	s.serveOutput(w, r, row, r.PathValue("name"))
}

// serveOutput sends only an output registered to the job.
func (s *Server) serveOutput(w http.ResponseWriter, r *http.Request, row *jobRow, name string) {
	allowed := false
	for _, output := range row.outputs() {
		if output.Name == name {
			allowed = true
		}
	}
	folder, ok := s.jobFolder(row.ID)
	if !allowed || !ok || name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		s.jsonError(w, r, http.StatusNotFound, "Output not found")
		return
	}
	s.serveFile(w, r, filepath.Join(folder, name), name, outputContentType(name))
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path, downloadName, contentType string) {
	file, err := os.Open(path)
	if err != nil {
		s.jsonError(w, r, http.StatusNotFound, "Output not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		s.jsonError(w, r, http.StatusNotFound, "Output not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(downloadName))
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (s *Server) handleDownloadAll(w http.ResponseWriter, r *http.Request, auth *authResult) {
	jobID := r.PathValue("job_id")
	row, err := s.ownedJob(r.Context(), jobID, auth.user)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if row == nil {
		s.jsonError(w, r, http.StatusNotFound, "Job not found")
		return
	}
	outputs := row.outputs()
	if len(outputs) == 0 {
		s.jsonError(w, r, http.StatusNotFound, "No outputs are ready")
		return
	}
	if len(outputs) == 1 {
		s.serveOutput(w, r, row, outputs[0].Name)
		return
	}
	folder, ok := s.jobFolder(row.ID)
	if !ok {
		s.jsonError(w, r, http.StatusNotFound, "Output not found")
		return
	}
	names := make([]string, len(outputs))
	for i, output := range outputs {
		if strings.ContainsAny(output.Name, `/\`) || output.Name == "" || output.Name == "." || output.Name == ".." {
			s.jsonError(w, r, http.StatusNotFound, "Output not found")
			return
		}
		names[i] = output.Name
	}
	// Key the cached archive by its exact contents: a ZIP requested while a
	// multi-language job is still running must not be served as the final one.
	digest := sha256.Sum256([]byte(pythonJSONStrings(names)))
	archive := filepath.Join(folder, ".translations-"+hex.EncodeToString(digest[:])[:24]+".zip")
	if _, err := os.Stat(archive); err != nil {
		if err := buildArchive(folder, names, archive); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				s.jsonError(w, r, http.StatusNotFound, "Output not found")
				return
			}
			s.internalError(w, r, err)
			return
		}
	}
	s.serveFile(w, r, archive, pathStem(row.Filename)+".translations.zip", "application/zip")
}

// buildArchive writes the ZIP under a private temporary name and publishes it
// atomically; concurrent requests never observe a partial archive.
func buildArchive(folder string, names []string, archive string) error {
	temporary, err := os.CreateTemp(folder, ".translations-*.partial")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	writer := zip.NewWriter(temporary)
	for _, name := range names {
		if err := addZipEntry(writer, filepath.Join(folder, name), name); err != nil {
			writer.Close()
			temporary.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Publish without ever replacing an existing archive: a concurrent request
	// may be sending it, and replacing an open file on Windows briefly leaves
	// the name unopenable. An existing archive for these names is identical.
	err = os.Link(temporaryName, archive)
	if err == nil || errors.Is(err, os.ErrExist) {
		return nil
	}
	// Filesystems without hard links: fall back to an atomic rename.
	if err := os.Rename(temporaryName, archive); err != nil {
		if _, statErr := os.Stat(archive); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func addZipEntry(writer *zip.Writer, path, name string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = name
	header.Method = zip.Deflate
	header.Modified = info.ModTime().Truncate(2 * time.Second)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, file)
	return err
}
