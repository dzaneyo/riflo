package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
)

func TestStoreCRUDAndNullableFields(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "tasks.sqlite")
	s := openTestStore(t, path)

	started := time.Date(2026, 8, 23, 1, 2, 3, 456789, time.FixedZone("CST", 8*60*60))
	created := time.Date(2026, 8, 23, 2, 3, 4, 567891234, time.FixedZone("CST", 8*60*60))
	task := app.Task{
		ID:            "task-1",
		SourceDisplay: "https://example.test/video?token=must-not-be-saved#fragment",
		OutputDir:     "/tmp/riflo-downloads",
		OutputName:    "video.mp4",
		Format:        "mp4",
		Status:        app.TaskQueued,
		Progress:      0.25,
		BytesDone:     42,
		CreatedAt:     created,
		StartedAt:     &started,
		UpdatedAt:     created,
	}
	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	expected := task
	expected.SourceDisplay = "https://example.test/video"
	assertTaskEqual(t, expected, got)
	if got.DurationMS != nil || got.FinishedAt != nil || got.ErrorCode != "" || got.ErrorMessage != "" {
		t.Fatalf("nullable fields were not kept null: %#v", got)
	}

	finished := created.Add(time.Minute)
	duration := int64(60000)
	task.SourceDisplay = "https://example.test/updated?signature=redact"
	task.Status = app.TaskSucceeded
	task.Progress = 1
	task.BytesDone = 1234
	task.DurationMS = &duration
	task.ErrorCode = ""
	task.ErrorMessage = ""
	task.FinishedAt = &finished
	task.UpdatedAt = finished
	if err := s.Update(ctx, task); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	expected = task
	expected.SourceDisplay = "https://example.test/updated"
	assertTaskEqual(t, expected, got)
}

func TestStoreListFilterLimitAndDescendingOrder(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "tasks.sqlite"))
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, status := range []app.TaskStatus{app.TaskSucceeded, app.TaskFailed, app.TaskSucceeded, app.TaskRunning} {
		created := base.Add(time.Duration(i) * time.Minute)
		task := newTestTask(fmt.Sprintf("task-%d", i), status, created)
		if err := s.Create(ctx, task); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	all, err := s.List(ctx, app.TaskFilter{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got := taskIDs(all); !reflect.DeepEqual(got, []string{"task-3", "task-2", "task-1", "task-0"}) {
		t.Fatalf("descending IDs = %#v", got)
	}
	limited, err := s.List(ctx, app.TaskFilter{Limit: 2})
	if err != nil {
		t.Fatalf("List limit: %v", err)
	}
	if got := taskIDs(limited); !reflect.DeepEqual(got, []string{"task-3", "task-2"}) {
		t.Fatalf("limited IDs = %#v", got)
	}
	succeeded, err := s.List(ctx, app.TaskFilter{Status: app.TaskSucceeded})
	if err != nil {
		t.Fatalf("List status: %v", err)
	}
	if got := taskIDs(succeeded); !reflect.DeepEqual(got, []string{"task-2", "task-0"}) {
		t.Fatalf("succeeded IDs = %#v", got)
	}
	if _, err := s.List(ctx, app.TaskFilter{Status: "invalid"}); err == nil {
		t.Fatal("List accepted an invalid status")
	}
	if _, err := s.List(ctx, app.TaskFilter{Limit: -1}); err == nil {
		t.Fatal("List accepted a negative limit")
	}
}

func TestStoreNotFoundAndReopenHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	ctx := context.Background()
	s := openTestStore(t, path)
	task := newTestTask("persisted", app.TaskQueued, time.Now().UTC())
	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, newTestTask("missing", app.TaskQueued, task.CreatedAt)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing error = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = openTestStore(t, path)
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	assertTaskEqual(t, task, got)
}

func TestStoreMarkInterrupted(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "tasks.sqlite"))
	ctx := context.Background()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, status := range []app.TaskStatus{app.TaskQueued, app.TaskRunning, app.TaskSucceeded, app.TaskFailed} {
		if err := s.Create(ctx, newTestTask(fmt.Sprintf("task-%d", i), status, base)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	count, err := s.MarkInterrupted(ctx)
	if err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}
	if count != 2 {
		t.Fatalf("MarkInterrupted count = %d, want 2", count)
	}
	for _, id := range []string{"task-0", "task-1"} {
		task, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if task.Status != app.TaskInterrupted || task.FinishedAt == nil || task.UpdatedAt.Before(base) {
			t.Fatalf("interrupted task %s = %#v", id, task)
		}
	}
	for _, id := range []string{"task-2", "task-3"} {
		task, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if task.Status == app.TaskInterrupted || task.FinishedAt != nil {
			t.Fatalf("non-running task %s changed = %#v", id, task)
		}
	}
	count, err = s.MarkInterrupted(ctx)
	if err != nil || count != 0 {
		t.Fatalf("second MarkInterrupted = (%d, %v), want (0, nil)", count, err)
	}
}

func TestStoreSchemaContainsNoSensitiveColumns(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "tasks.sqlite"))
	rows, err := s.db.Query("PRAGMA table_info(tasks)")
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	columns := make([]string, 0)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	expected := []string{
		"id", "source_display", "output_dir", "output_name", "format", "status",
		"progress", "bytes_done", "duration_ms", "error_code", "error_message",
		"created_at", "started_at", "finished_at", "updated_at",
	}
	if !reflect.DeepEqual(columns, expected) {
		t.Fatalf("schema columns = %#v, want %#v", columns, expected)
	}
	for _, column := range columns {
		for _, sensitive := range []string{"url", "referer", "origin", "user_agent", "cookie", "token", "command", "authorization"} {
			if column == sensitive {
				t.Fatalf("sensitive schema column %q exists", column)
			}
		}
	}
}

func TestStoreConcurrentCreates(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "tasks.sqlite"))
	const count = 24
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := newTestTask(fmt.Sprintf("concurrent-%02d", i), app.TaskQueued, time.Now().UTC())
			if err := s.Create(ctx, task); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Create: %v", err)
	}
	tasks, err := s.List(ctx, app.TaskFilter{})
	if err != nil {
		t.Fatalf("List after concurrent creates: %v", err)
	}
	if len(tasks) != count {
		t.Fatalf("concurrent task count = %d, want %d", len(tasks), count)
	}
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestTask(id string, status app.TaskStatus, created time.Time) app.Task {
	return app.Task{
		ID:            id,
		SourceDisplay: "https://example.test/" + id,
		OutputDir:     "/tmp/downloads",
		OutputName:    id + ".mp4",
		Format:        "mp4",
		Status:        status,
		CreatedAt:     created,
		UpdatedAt:     created,
	}
}

func taskIDs(tasks []app.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func assertTaskEqual(t *testing.T, expected, got app.Task) {
	t.Helper()
	if !expected.CreatedAt.Equal(got.CreatedAt) || !expected.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("timestamps differ: expected %#v, got %#v", expected, got)
	}
	if expected.StartedAt == nil != (got.StartedAt == nil) || expected.FinishedAt == nil != (got.FinishedAt == nil) {
		t.Fatalf("nullable timestamps differ: expected %#v, got %#v", expected, got)
	}
	if expected.StartedAt != nil && !expected.StartedAt.Equal(*got.StartedAt) {
		t.Fatalf("started_at differs: expected %v, got %v", *expected.StartedAt, *got.StartedAt)
	}
	if expected.FinishedAt != nil && !expected.FinishedAt.Equal(*got.FinishedAt) {
		t.Fatalf("finished_at differs: expected %v, got %v", *expected.FinishedAt, *got.FinishedAt)
	}
	expected.CreatedAt = time.Time{}
	got.CreatedAt = time.Time{}
	expected.UpdatedAt = time.Time{}
	got.UpdatedAt = time.Time{}
	if expected.StartedAt != nil {
		value := time.Time{}
		expected.StartedAt = &value
	}
	if got.StartedAt != nil {
		value := time.Time{}
		got.StartedAt = &value
	}
	if expected.FinishedAt != nil {
		value := time.Time{}
		expected.FinishedAt = &value
	}
	if got.FinishedAt != nil {
		value := time.Time{}
		got.FinishedAt = &value
	}
	if !reflect.DeepEqual(expected, got) {
		t.Fatalf("task differs: expected %#v, got %#v", expected, got)
	}
}
