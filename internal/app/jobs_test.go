package app

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AutisticShark/Subtitle-Translator/internal/engine"
)

// blockingTranslator reports progress, then waits until released or canceled.
type blockingTranslator struct {
	started chan string
	release chan struct{}
	fail    error
	mu      sync.Mutex
	configs []engine.Config
}

func newBlockingTranslator() *blockingTranslator {
	return &blockingTranslator{started: make(chan string, 16), release: make(chan struct{})}
}

func (b *blockingTranslator) CountCues(source []byte, extension, encoding string) (int, error) {
	return engine.CountCues(source, extension, encoding)
}

func (b *blockingTranslator) Translate(ctx context.Context, source []byte, extension, target string,
	config engine.Config, progress func(done, total int)) ([]byte, engine.Stats, error) {
	b.mu.Lock()
	b.configs = append(b.configs, config)
	b.mu.Unlock()
	progress(1, 4)
	b.started <- target
	select {
	case <-ctx.Done():
		return nil, engine.Stats{}, context.Cause(ctx)
	case <-b.release:
	}
	if b.fail != nil {
		return nil, engine.Stats{}, b.fail
	}
	return []byte("translated " + target), engine.Stats{Cues: 2, Segments: 2, Fallbacks: 1}, nil
}

func withTranslator(translator Translator) option {
	return func(c *Config) { c.Translator = translator }
}

func TestEchoJobEndToEndWithDownloadsAndArchive(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	result := admin.upload(map[string]string{"provider": "echo", "target_languages": "ja, zh-TW,ja"},
		map[string]string{"字幕 第1話.en.srt": testSubtitle})
	expectStatus(t, result, http.StatusAccepted)
	jobID := jobIDs(t, result)[0]
	job := env.waitForJob(admin, jobID)
	if job["status"] != "completed" || job["progress"] != float64(100) || job["stage"] != "Ready to download" {
		t.Fatalf("job %v", job)
	}
	options := job["options"].(map[string]any)
	if len(options["target_languages"].([]any)) != 2 || options["encoding"] != "utf-8" {
		t.Fatalf("options %v", options)
	}
	outputs := job["outputs"].([]any)
	names := []string{outputs[0].(map[string]any)["name"].(string), outputs[1].(map[string]any)["name"].(string)}
	if names[0] != "字幕 第1話.ja.srt" || names[1] != "字幕 第1話.zh.tw.srt" {
		t.Fatalf("output names %v", names)
	}
	if _, err := os.Stat(filepath.Join(env.server.jobsDir, jobID, "source.srt")); err != nil {
		t.Fatal("source not stored as source.srt")
	}
	download := admin.get("/api/jobs/" + jobID + "/download/" + url.PathEscape(names[0]))
	expectStatus(t, download, http.StatusOK)
	if !strings.Contains(string(download.body), "[ja] Hello there") {
		t.Fatalf("translated output %q", download.body)
	}
	if !strings.Contains(download.header.Get("Content-Disposition"), "filename*=UTF-8''") {
		t.Fatalf("disposition %q", download.header.Get("Content-Disposition"))
	}
	expectStatus(t, admin.get("/api/jobs/"+jobID+"/download/source.srt"), http.StatusNotFound)
	expectStatus(t, admin.get("/api/jobs/"+jobID+"/download/..%2F..%2Fapp.db"), http.StatusNotFound)
	archive := admin.get("/api/jobs/" + jobID + "/download")
	expectStatus(t, archive, http.StatusOK)
	reader, err := zip.NewReader(bytes.NewReader(archive.body), int64(len(archive.body)))
	if err != nil || len(reader.File) != 2 || reader.File[0].Name != names[0] {
		t.Fatalf("archive: %v", err)
	}
	if !strings.Contains(archive.header.Get("Content-Disposition"), url.PathEscape("字幕 第1話.en.translations.zip")) {
		t.Fatalf("archive name %q", archive.header.Get("Content-Disposition"))
	}
	// Terminal jobs can be deleted with their files.
	expectStatus(t, admin.do(http.MethodDelete, "/api/jobs/"+jobID, nil, ""), http.StatusOK)
	if _, err := os.Stat(filepath.Join(env.server.jobsDir, jobID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("job folder left behind")
	}
	expectStatus(t, admin.get("/api/jobs/"+jobID), http.StatusNotFound)
}

func TestConcurrentArchiveDownloadsNeverServeAPartialZip(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	jobID := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja,ko,fr"},
		map[string]string{"show.srt": testSubtitle}))[0]
	env.waitForJob(admin, jobID)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := admin.get("/api/jobs/" + jobID + "/download")
			body := result.body
			if reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil || len(reader.File) != 3 {
				t.Errorf("partial archive: %v (status %d, %.200s)", err, result.status, body)
			}
		}()
	}
	wg.Wait()
	entries, _ := os.ReadDir(filepath.Join(env.server.jobsDir, jobID))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".partial") {
			t.Fatalf("temporary archive left: %s", entry.Name())
		}
	}
}

func TestJobValidationRejectsBeforeQueueing(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	cases := []struct {
		fields map[string]string
		files  map[string]string
	}{
		{map[string]string{"provider": "echo"}, nil},
		{map[string]string{"provider": "echo"}, map[string]string{"notes.txt": "x"}},
		{map[string]string{"provider": "bogus"}, map[string]string{"a.srt": testSubtitle}},
		{map[string]string{"provider": "echo", "target_languages": "xx"}, map[string]string{"a.srt": testSubtitle}},
		{map[string]string{"provider": "echo", "target_languages": " , "}, map[string]string{"a.srt": testSubtitle}},
	}
	for _, item := range cases {
		expectStatus(t, admin.upload(item.fields, item.files), http.StatusBadRequest)
	}
	if env.scalar("SELECT COUNT(*) FROM jobs") != 0 {
		t.Fatal("rejected submissions created jobs")
	}
	entries, _ := os.ReadDir(env.server.jobsDir)
	if len(entries) != 0 {
		t.Fatal("rejected submissions left folders")
	}
}

func TestEchoIsRejectedWithoutDebugMode(t *testing.T) {
	env := newEnv(t, func(c *Config) { c.Debug = false })
	admin := env.setupAdmin()
	settings := admin.get("/api/settings").json()
	if _, ok := settings["providers"].(map[string]any)["echo"]; ok {
		t.Fatal("echo offered without debug")
	}
	expectStatus(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}), http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"default_provider": "echo"}),
		http.StatusBadRequest)
}

func TestProviderWithoutKeyFailsAndErrorsAreAdminOnly(t *testing.T) {
	env := newEnv(t, func(c *Config) { c.Debug = false })
	admin := env.setupAdmin()
	member := env.createUser(admin, "member", "user")
	jobID := jobIDs(t, member.upload(map[string]string{"provider": "anthropic", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	job := env.waitForJob(member, jobID)
	if job["status"] != "failed" || job["stage"] != "Translation failed" {
		t.Fatalf("job %v", job)
	}
	if strings.Contains(job["error"].(string), "API key") {
		t.Fatalf("submitter saw a provider error: %v", job["error"])
	}
	adminView := admin.get("/api/jobs/" + jobID).json()
	if adminView["error"] != "FatalTranslationError: Anthropic API key is not configured" {
		t.Fatalf("admin error %v", adminView["error"])
	}
	// File problems stay visible to the submitter.
	broken := jobIDs(t, member.upload(map[string]string{"provider": "anthropic", "target_languages": "ja",
		"encoding": "utf-8"}, map[string]string{"b.srt": "\xff\xfe\xfa broken"}))[0]
	job = env.waitForJob(member, broken)
	if job["status"] != "failed" || !strings.HasPrefix(job["error"].(string), "SubtitleFormatError: ") {
		t.Fatalf("format error %v", job["error"])
	}
}

func TestUsersSeeOnlyTheirOwnJobs(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	alice := env.createUser(admin, "alice", "user")
	bob := env.createUser(admin, "bobby", "user")
	jobID := jobIDs(t, alice.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	env.waitForJob(alice, jobID)
	for _, path := range []string{"/api/jobs/" + jobID, "/api/jobs/" + jobID + "/download"} {
		expectStatus(t, bob.get(path), http.StatusNotFound)
	}
	expectStatus(t, bob.post("/api/jobs/"+jobID+"/cancel", map[string]any{}), http.StatusNotFound)
	expectStatus(t, bob.do(http.MethodDelete, "/api/jobs/"+jobID, nil, ""), http.StatusNotFound)
	if list := bob.get("/api/jobs").json(); list["total"] != float64(0) {
		t.Fatalf("bob sees %v", list)
	}
	// all=1 is ignored for regular users and honored for administrators.
	if list := bob.get("/api/jobs?all=1").json(); list["total"] != float64(0) {
		t.Fatal("regular user listed all jobs")
	}
	all := admin.get("/api/jobs?all=1&limit=abc&offset=-5").json()
	if all["total"] != float64(1) || all["limit"] != float64(50) || all["offset"] != float64(0) {
		t.Fatalf("admin list %v", all)
	}
	if all["jobs"].([]any)[0].(map[string]any)["owner"] != "alice" {
		t.Fatal("owner missing in admin listing")
	}
	if all["counts"].(map[string]any)["completed"] != float64(1) || all["has_more"] != false {
		t.Fatalf("counts %v", all["counts"])
	}
	expectStatus(t, admin.get("/api/jobs/"+jobID), http.StatusOK)
	// An administrator cannot delete a user who still has active jobs.
	aliceID := alice.get("/api/auth/me").json()["user"].(map[string]any)["id"].(string)
	expectStatus(t, admin.do(http.MethodDelete, "/api/users/"+aliceID, nil, ""), http.StatusOK)
	if env.text("SELECT user_id FROM jobs WHERE id = ?", jobID) != "" {
		t.Fatal("deleted user's job kept its owner")
	}
}

func TestCancellationOfRunningAndQueuedJobs(t *testing.T) {
	translator := newBlockingTranslator()
	env := newEnv(t, withTranslator(translator), func(c *Config) { c.JobWorkers = 1 })
	admin := env.setupAdmin()
	ids := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja,ko"},
		map[string]string{"one.srt": testSubtitle, "two.srt": testSubtitle}))
	<-translator.started
	var running, queued string
	for _, id := range ids {
		if admin.get("/api/jobs/" + id).json()["status"] == "processing" {
			running = id
		} else {
			queued = id
		}
	}
	if running == "" || queued == "" {
		t.Fatalf("expected one running and one queued job: %v", ids)
	}
	// Active jobs cannot be deleted.
	expectStatus(t, admin.do(http.MethodDelete, "/api/jobs/"+running, nil, ""), http.StatusConflict)
	// A queued job becomes terminal at once.
	result := admin.post("/api/jobs/"+queued+"/cancel", map[string]any{})
	expectStatus(t, result, http.StatusAccepted)
	if result.str("status") != "canceled" {
		t.Fatalf("queued cancel %s", result.body)
	}
	// A running job moves to canceling and its worker finalizes it.
	result = admin.post("/api/jobs/"+running+"/cancel", map[string]any{})
	expectStatus(t, result, http.StatusAccepted)
	if result.str("status") != "canceling" {
		t.Fatalf("running cancel %s", result.body)
	}
	job := env.waitForJob(admin, running)
	if job["status"] != "canceled" || job["stage"] != "Canceled" {
		t.Fatalf("job %v", job)
	}
	expectStatus(t, admin.post("/api/jobs/"+running+"/cancel", map[string]any{}), http.StatusConflict)
	expectStatus(t, admin.post("/api/jobs/missing/cancel", map[string]any{}), http.StatusNotFound)
	// The canceled queued job never ran.
	select {
	case target := <-translator.started:
		t.Fatalf("canceled queued job started (%s)", target)
	case <-time.After(300 * time.Millisecond):
	}
	expectStatus(t, admin.do(http.MethodDelete, "/api/jobs/"+running, nil, ""), http.StatusOK)
}

func TestWorkerStopsWhenAnotherProcessRecordsCancellation(t *testing.T) {
	translator := newBlockingTranslator()
	previous := cancelPollInterval
	cancelPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { cancelPollInterval = previous })
	env := newEnv(t, withTranslator(translator))
	admin := env.setupAdmin()
	jobID := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	<-translator.started
	env.exec("UPDATE jobs SET status = 'canceling', stage = 'Canceling' WHERE id = ?", jobID)
	job := env.waitForJob(admin, jobID)
	if job["status"] != "canceled" {
		t.Fatalf("job %v", job)
	}
}

func TestCompletionCannotOverwriteACancellation(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	jobID := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	env.waitForJob(admin, jobID)
	env.exec("UPDATE jobs SET status = 'canceling' WHERE id = ?", jobID)
	changed, err := env.server.writeJob(context.Background(), jobID, []string{"processing"},
		map[string]any{"status": "completed"})
	if err != nil || changed {
		t.Fatalf("conditional write changed=%v err=%v", changed, err)
	}
	if env.text("SELECT status FROM jobs WHERE id = ?", jobID) != "canceling" {
		t.Fatal("completion overwrote the cancellation")
	}
}

func TestProgressWarningsAndProviderSettingsReachTheEngine(t *testing.T) {
	translator := newBlockingTranslator()
	env := newEnv(t, withTranslator(translator))
	admin := env.setupAdmin()
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{
		"openai_api_key": "sk-test", "openai_model": "gpt-test", "openai_base_url": "https://llm.example/v1",
		"batch_size": 7, "workers": "3", "rpm": "12.5", "width": 20, "max_lines": "3",
	}), http.StatusOK)
	jobID := jobIDs(t, admin.upload(map[string]string{"provider": "openai", "target_languages": "ja,ko",
		"source_language": " Japanese ", "encoding": "big5"}, map[string]string{"a.SRT": testSubtitle}))[0]
	<-translator.started
	job := admin.get("/api/jobs/" + jobID).json()
	if !strings.HasPrefix(job["stage"].(string), "Translating to Japanese (1/4 segments)") || job["progress"] != float64(16) {
		t.Fatalf("progress %v %v", job["stage"], job["progress"])
	}
	close(translator.release)
	job = env.waitForJob(admin, jobID)
	if job["status"] != "completed" || job["warning"] != "Left untranslated: 2 of 4 segments" {
		t.Fatalf("job %v", job)
	}
	translator.mu.Lock()
	config := translator.configs[0]
	translator.mu.Unlock()
	if config.APIKey != "sk-test" || config.Model != "gpt-test" || config.BaseURL != "https://llm.example/v1" ||
		config.BatchSize != 7 || config.Workers != 3 || config.RPM != 12.5 || config.Width != 20 ||
		config.MaxLines != 3 || config.Encoding != "big5" || config.SourceLang != "Japanese" {
		t.Fatalf("engine config %+v", config)
	}
	names := job["outputs"].([]any)
	if names[0].(map[string]any)["name"] != "a.ja.srt" {
		t.Fatalf("outputs %v", names)
	}
	// Localized stage and warning for another viewer locale.
	admin.header["Accept-Language"] = "zh-TW"
	localized := admin.get("/api/jobs/" + jobID).json()
	if localized["stage"] == "Ready to download" || localized["warning"] == job["warning"] {
		t.Fatalf("not localized: %v", localized)
	}
}

func TestQuotasRejectMultiFileUploadsAtomically(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	member := env.createUser(admin, "member", "user")
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"user_job_limit": "2",
		"rate_limit_window_minutes": "60"}), http.StatusOK)
	files := map[string]string{"a.srt": testSubtitle, "b.srt": testSubtitle, "c.srt": testSubtitle}
	result := member.upload(map[string]string{"provider": "echo", "target_languages": "ja"}, files)
	expectStatus(t, result, http.StatusTooManyRequests)
	body := result.json()
	if body["scope"] != "user" || body["limit"] != float64(2) || body["requested"] != float64(3) ||
		result.header.Get("Retry-After") == "" {
		t.Fatalf("quota response %v %v", body, result.header)
	}
	if env.scalar("SELECT COUNT(*) FROM jobs") != 0 || env.scalar("SELECT COUNT(*) FROM rate_limit_buckets") != 0 {
		t.Fatal("rejected upload consumed quota or created jobs")
	}
	ids := jobIDs(t, member.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle, "b.srt": testSubtitle}))
	for _, id := range ids {
		env.waitForJob(member, id)
	}
	expectStatus(t, member.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}), http.StatusTooManyRequests)
	// Administrators have their own limit.
	expectStatus(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}), http.StatusAccepted)
	// The window reopens.
	env.clock.advance(61 * time.Minute)
	member = env.login("member")
	expectStatus(t, member.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}), http.StatusAccepted)
}

func TestConcurrentSubmissionsNeverExceedThePanelLimit(t *testing.T) {
	env := newEnv(t, withTranslator(newBlockingTranslator()))
	admin := env.setupAdmin()
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"panel_daily_job_limit": "5"}),
		http.StatusOK)
	users := []*client{admin}
	for _, name := range []string{"one", "two", "three"} {
		users = append(users, env.createUser(admin, name, "user"))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := users[i%len(users)].upload(map[string]string{"provider": "echo", "target_languages": "ja"},
				map[string]string{"x.srt": testSubtitle})
			if result.status == http.StatusAccepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 5 || env.scalar("SELECT COUNT(*) FROM jobs") != 5 {
		t.Fatalf("accepted %d jobs, %d rows", accepted, env.scalar("SELECT COUNT(*) FROM jobs"))
	}
	if env.scalar("SELECT used FROM rate_limit_buckets WHERE scope = 'panel:daily'") != 5 {
		t.Fatal("panel bucket out of step")
	}
}

func TestStartupRecoveryAndCloseFinalizeActiveJobs(t *testing.T) {
	translator := newBlockingTranslator()
	env := newEnv(t, withTranslator(translator), func(c *Config) { c.JobWorkers = 1 })
	admin := env.setupAdmin()
	ids := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle, "b.srt": testSubtitle}))
	<-translator.started
	// Close interrupts the running job and finalizes the queued one.
	env.restart()
	for _, id := range ids {
		if status := env.text("SELECT status FROM jobs WHERE id = ?", id); status != "failed" {
			t.Fatalf("job %s left %s", id, status)
		}
		if env.text("SELECT error FROM jobs WHERE id = ?", id) != recoveredJobError {
			t.Fatal("recovery error missing")
		}
	}
	// Rows a crashed process left behind are recovered at startup.
	timestamp := isoSeconds(time.Now())
	for id, status := range map[string]string{"q": "queued", "p": "processing", "c": "canceling", "d": "completed"} {
		env.exec("INSERT INTO jobs (id, user_id, filename, stored_name, status, progress, stage, options, outputs, "+
			"created_at, updated_at) VALUES (?, NULL, 'a.srt', 'source.srt', ?, 0, '', '{}', '[]', ?, ?)",
			id, status, timestamp, timestamp)
	}
	env.restart()
	want := map[string]string{"q": "failed", "p": "failed", "c": "canceled", "d": "completed"}
	for id, status := range want {
		if got := env.text("SELECT status FROM jobs WHERE id = ?", id); got != status {
			t.Fatalf("job %s: %s, want %s", id, got, status)
		}
	}
	env.restart(func(c *Config) { c.SkipRecovery = true })
}

func TestUploadsOverTheLimitAreRejected(t *testing.T) {
	env := newEnv(t, func(c *Config) { c.MaxUploadMB = 1 })
	admin := env.setupAdmin()
	result := admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"big.srt": strings.Repeat("x", 2<<20)})
	expectStatus(t, result, http.StatusRequestEntityTooLarge)
	if !strings.Contains(result.str("error"), "1 MB") {
		t.Fatalf("error %q", result.str("error"))
	}
	if env.scalar("SELECT COUNT(*) FROM jobs") != 0 {
		t.Fatal("oversized upload created a job")
	}
}
