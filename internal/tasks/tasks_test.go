package tasks

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/media"
	"github.com/dzaneyo/riflo/internal/store"
)

type fakeRunner struct {
	inspect func(context.Context, app.InspectRequest) (app.MediaInfo, error)
	dl      func(context.Context, string, app.CreateTaskRequest, func(media.Progress)) (media.Result, error)
}

func (f *fakeRunner) Inspect(ctx context.Context, req app.InspectRequest) (app.MediaInfo, error) {
	if f.inspect != nil {
		return f.inspect(ctx, req)
	}
	return app.MediaInfo{}, nil
}

func (f *fakeRunner) Download(ctx context.Context, id string, req app.CreateTaskRequest, report func(media.Progress)) (media.Result, error) {
	if f.dl != nil {
		return f.dl(ctx, id, req, report)
	}
	return media.Result{}, nil
}

func openTasksTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tasks.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestService(t *testing.T, runner mediaRunner, concurrency int) *Service {
	t.Helper()
	s, err := newWithRunner(context.Background(), Config{
		Store:            openTasksTestStore(t),
		DefaultOutputDir: t.TempDir(),
		Concurrency:      concurrency,
	}, runner)
	if err != nil {
		t.Fatalf("newWithRunner: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNewMarksInterruptedAndCreatesSafeDefaults(t *testing.T) {
	db := openTasksTestStore(t)
	for _, status := range []app.TaskStatus{app.TaskQueued, app.TaskRunning, app.TaskSucceeded} {
		now := time.Now().UTC()
		if err := db.Create(context.Background(), app.Task{
			ID:            "old-" + string(status),
			SourceDisplay: "https://example.test/video",
			OutputDir:     t.TempDir(),
			OutputName:    "old.mp4",
			Format:        "mp4",
			Status:        status,
			CreatedAt:     now,
			UpdatedAt:     now,
		}); err != nil {
			t.Fatalf("Create(%s): %v", status, err)
		}
	}
	s, err := newWithRunner(context.Background(), Config{Store: db, Concurrency: 0}, &fakeRunner{})
	if err != nil {
		t.Fatalf("newWithRunner: %v", err)
	}
	defer s.Close()
	for _, status := range []app.TaskStatus{app.TaskQueued, app.TaskRunning} {
		got, err := db.Get(context.Background(), "old-"+string(status))
		if err != nil {
			t.Fatalf("Get(%s): %v", status, err)
		}
		if got.Status != app.TaskInterrupted || got.FinishedAt == nil {
			t.Fatalf("old %s task = %+v, want interrupted", status, got)
		}
	}

	task, err := s.CreateTask(context.Background(), app.CreateTaskRequest{
		URL:    "https://media.example/video/master.m3u8?token=secret#fragment",
		Cookie: "session=secret",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Status != app.TaskQueued || !strings.HasPrefix(task.OutputName, "download-") || !strings.HasSuffix(task.OutputName, ".mp4") {
		t.Fatalf("default task = %+v", task)
	}
	if task.SourceDisplay != "https://media.example/video/master.m3u8" {
		t.Fatalf("source display = %q", task.SourceDisplay)
	}
}

func TestCreateSuccessProgressAndSensitiveMetadata(t *testing.T) {
	var seen app.CreateTaskRequest
	runner := &fakeRunner{dl: func(_ context.Context, _ string, req app.CreateTaskRequest, report func(media.Progress)) (media.Result, error) {
		seen = req
		report(media.Progress{Fraction: 0.15, BytesDone: 100, DurationMS: 4000})
		report(media.Progress{Fraction: 0.8, BytesDone: 200, DurationMS: 4000})
		return media.Result{DurationMS: 3200}, nil
	}}
	s := newTestService(t, runner, 1)
	task, err := s.CreateTask(context.Background(), app.CreateTaskRequest{
		URL:        "https://example.test/video.m3u8?token=do-not-store#frag",
		Referer:    "https://example.test/watch?id=private",
		UserAgent:  "private-agent",
		Cookie:     "session=private",
		OutputName: "saved",
		Format:     "mkv",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got := waitForTask(t, s, task.ID, func(value app.Task) bool { return value.Status == app.TaskSucceeded })
	if got.Progress != 1 || got.BytesDone != 200 || got.DurationMS == nil || *got.DurationMS != 3200 {
		t.Fatalf("completed task = %+v", got)
	}
	if got.SourceDisplay != "https://example.test/video.m3u8" || strings.Contains(got.SourceDisplay, "token") {
		t.Fatalf("sensitive source display = %q", got.SourceDisplay)
	}
	if seen.URL == "" || seen.Cookie != "session=private" || seen.Referer == "" {
		t.Fatalf("runner did not receive transient request: %+v", seen)
	}
	if strings.Contains(got.ErrorMessage, "private") || strings.Contains(got.ErrorMessage, "token") {
		t.Fatalf("unexpected sensitive error = %q", got.ErrorMessage)
	}
}

func TestFailureRedactsSensitiveErrorAndBoundsMessage(t *testing.T) {
	secretURL := "https://example.test/video.m3u8?token=secret-token"
	secretCookie := "session=super-secret"
	secretAgent := "private-agent"
	runner := &fakeRunner{dl: func(context.Context, string, app.CreateTaskRequest, func(media.Progress)) (media.Result, error) {
		return media.Result{}, fmt.Errorf("request %s Cookie: %s User-Agent: %s %s", secretURL, secretCookie, secretAgent, strings.Repeat("x", 1000))
	}}
	s := newTestService(t, runner, 1)
	task, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: secretURL, Cookie: secretCookie, UserAgent: secretAgent})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got := waitForTask(t, s, task.ID, func(value app.Task) bool { return value.Status == app.TaskFailed })
	for _, secret := range []string{secretURL, "secret-token", secretCookie, secretAgent} {
		if strings.Contains(got.ErrorMessage, secret) {
			t.Fatalf("error leaked %q: %q", secret, got.ErrorMessage)
		}
	}
	if len(got.ErrorMessage) > maxErrorMessage {
		t.Fatalf("error length = %d, want <= %d", len(got.ErrorMessage), maxErrorMessage)
	}
}

func TestQueuedAndRunningCancellation(t *testing.T) {
	started := make(chan struct{})
	runner := &fakeRunner{dl: func(ctx context.Context, _ string, _ app.CreateTaskRequest, _ func(media.Progress)) (media.Result, error) {
		close(started)
		<-ctx.Done()
		return media.Result{}, ctx.Err()
	}}
	s := newTestService(t, runner, 1)
	first, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: "https://example.test/first"})
	if err != nil {
		t.Fatalf("CreateTask first: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first task did not start")
	}
	second, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: "https://example.test/second"})
	if err != nil {
		t.Fatalf("CreateTask second: %v", err)
	}
	canceled, err := s.CancelTask(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("CancelTask queued: %v", err)
	}
	if canceled.Status != app.TaskCanceled {
		t.Fatalf("queued cancel = %+v", canceled)
	}
	if _, err := s.CancelTask(context.Background(), first.ID); err != nil {
		t.Fatalf("CancelTask running: %v", err)
	}
	got := waitForTask(t, s, first.ID, func(value app.Task) bool { return value.Status == app.TaskCanceled })
	if got.ErrorCode != "canceled" || got.FinishedAt == nil {
		t.Fatalf("running cancel = %+v", got)
	}
	terminal, err := s.CancelTask(context.Background(), first.ID)
	if err != nil || terminal.Status != app.TaskCanceled {
		t.Fatalf("terminal cancel = %+v, %v", terminal, err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	var active, maxActive atomic.Int32
	release := make(chan struct{})
	runner := &fakeRunner{dl: func(_ context.Context, _ string, _ app.CreateTaskRequest, report func(media.Progress)) (media.Result, error) {
		current := active.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		<-release
		active.Add(-1)
		report(media.Progress{Fraction: 1, BytesDone: 1})
		return media.Result{DurationMS: 1}, nil
	}}
	s := newTestService(t, runner, 2)
	ids := make([]string, 4)
	for i := range ids {
		task, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: fmt.Sprintf("https://example.test/%d", i)})
		if err != nil {
			t.Fatalf("CreateTask %d: %v", i, err)
		}
		ids[i] = task.ID
	}
	deadline := time.Now().Add(2 * time.Second)
	for maxActive.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if maxActive.Load() != 2 {
		t.Fatalf("max active = %d, want 2", maxActive.Load())
	}
	close(release)
	for _, id := range ids {
		waitForTask(t, s, id, func(value app.Task) bool { return value.Status == app.TaskSucceeded })
	}
}

func TestInspectMapsAndRedactsErrors(t *testing.T) {
	secretURL := "https://example.test/inspect?sig=secret"
	secretCookie := "session=secret"
	runner := &fakeRunner{inspect: func(context.Context, app.InspectRequest) (app.MediaInfo, error) {
		return app.MediaInfo{}, fmt.Errorf("ffprobe failed for %s Cookie: %s", secretURL, secretCookie)
	}}
	s := newTestService(t, runner, 1)
	_, err := s.Inspect(context.Background(), app.InspectRequest{URL: secretURL, Cookie: secretCookie})
	if err == nil {
		t.Fatal("Inspect unexpectedly succeeded")
	}
	var appErr *app.Error
	if !errors.As(err, &appErr) || appErr.Code != "inspect_failed" {
		t.Fatalf("Inspect error = %T %v", err, err)
	}
	for _, secret := range []string{secretURL, "secret", secretCookie} {
		if strings.Contains(appErr.Message, secret) {
			t.Fatalf("Inspect error leaked %q: %q", secret, appErr.Message)
		}
	}
}

func TestInspectMapsMediaAccessDenied(t *testing.T) {
	runner := &fakeRunner{inspect: func(context.Context, app.InspectRequest) (app.MediaInfo, error) {
		return app.MediaInfo{}, fmt.Errorf("probe: %w", media.ErrAccessDenied)
	}}
	service := newTestService(t, runner, 1)
	defer service.Close()
	_, err := service.Inspect(context.Background(), app.InspectRequest{URL: "https://media.example.test/video.m3u8?token=secret"})
	if err == nil {
		t.Fatal("Inspect unexpectedly succeeded")
	}
	var appErr *app.Error
	if !errors.As(err, &appErr) || appErr.Code != "media_access_denied" || appErr.Status != 502 {
		t.Fatalf("Inspect error = %T %v", err, err)
	}
	if !strings.Contains(appErr.Message, "Referer") || !strings.Contains(appErr.Message, "Cookie") || strings.Contains(appErr.Message, "secret") {
		t.Fatalf("access error message = %q", appErr.Message)
	}
}

func TestCloseCancelsQueuedAndRejectsNewTasks(t *testing.T) {
	started := make(chan struct{})
	runner := &fakeRunner{dl: func(ctx context.Context, _ string, _ app.CreateTaskRequest, _ func(media.Progress)) (media.Result, error) {
		close(started)
		<-ctx.Done()
		return media.Result{}, ctx.Err()
	}}
	db := openTasksTestStore(t)
	s, err := newWithRunner(context.Background(), Config{Store: db, DefaultOutputDir: t.TempDir(), Concurrency: 1}, runner)
	if err != nil {
		t.Fatalf("newWithRunner: %v", err)
	}
	first, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: "https://example.test/first"})
	if err != nil {
		t.Fatalf("CreateTask first: %v", err)
	}
	second, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: "https://example.test/second"})
	if err != nil {
		t.Fatalf("CreateTask second: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("task did not start")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		got, err := db.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if got.Status != app.TaskCanceled {
			t.Fatalf("closed task %s = %+v", id, got)
		}
	}
	if _, err := s.CreateTask(context.Background(), app.CreateTaskRequest{URL: "https://example.test/new"}); err == nil {
		t.Fatal("CreateTask succeeded after Close")
	}
}

func waitForTask(t *testing.T, service *Service, id string, done func(app.Task) bool) app.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, err := service.GetTask(context.Background(), id)
		if err == nil && done(task) {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	task, err := service.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("wait task %s: %v", id, err)
	}
	t.Fatalf("task %s did not reach expected state: %+v", id, task)
	return app.Task{}
}
