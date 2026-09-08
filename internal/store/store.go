// Package store provides the SQLite-backed task history used by riflo.
//
// The store deliberately contains only non-sensitive task metadata. Request
// URLs, authentication headers, cookies, and generated media commands belong
// to the lifetime of a task execution and must not be passed to this package.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested task does not exist.
var ErrNotFound = errors.New("task not found")

const (
	openTimeout   = 10 * time.Second
	busyTimeout   = 5000
	schemaVersion = 1
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    id             TEXT PRIMARY KEY,
    source_display TEXT NOT NULL,
    output_dir     TEXT NOT NULL,
    output_name    TEXT NOT NULL,
    format         TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled', 'interrupted')),
    progress       REAL NOT NULL DEFAULT 0,
    bytes_done     INTEGER NOT NULL DEFAULT 0,
    duration_ms    INTEGER,
    error_code     TEXT,
    error_message  TEXT,
    created_at     TEXT NOT NULL,
    started_at     TEXT,
    finished_at    TEXT,
    updated_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_status_created_at
    ON tasks (status, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at
    ON tasks (created_at DESC, id DESC);
`

const taskColumns = `
    id,
    source_display,
    output_dir,
    output_name,
    format,
    status,
    progress,
    bytes_done,
    duration_ms,
    error_code,
    error_message,
    created_at,
    started_at,
    finished_at,
    updated_at
`

// Store is a small serialized SQLite store. SQLite is configured in WAL mode
// for safe readers while a task is being updated; writes are serialized with a
// single pool connection so callers do not need to coordinate database locks.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) a task database at path and initializes its schema.
// Parent directories are created with private permissions. The special
// SQLite in-memory paths are accepted for tests and are not chmod'ed.
func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("database path is required")
	}

	if !isMemoryPath(path) && !isURIPath(path) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	// A single connection keeps PRAGMA settings consistent and avoids lock
	// contention for the small local-first workload. WAL still permits a
	// separate process to read a stable snapshot while this process writes.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	closeDB := func(openErr error) (*Store, error) {
		_ = db.Close()
		return nil, openErr
	}

	if err := db.PingContext(ctx); err != nil {
		return closeDB(fmt.Errorf("ping sqlite database: %w", err))
	}
	for _, pragma := range []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout),
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return closeDB(fmt.Errorf("configure sqlite (%s): %w", pragma, err))
		}
	}
	if err := migrate(ctx, db); err != nil {
		return closeDB(fmt.Errorf("initialize sqlite schema: %w", err))
	}

	// Keep the main database private even when the process umask is permissive.
	// SQLite creates -wal/-shm alongside it as needed; those files inherit the
	// process umask and contain the same local task metadata.
	if !isMemoryPath(path) && !isURIPath(path) {
		if err := os.Chmod(path, 0o600); err != nil {
			return closeDB(fmt.Errorf("secure sqlite database: %w", err))
		}
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Create inserts a task into history. It performs one atomic SQL statement.
func (s *Store) Create(ctx context.Context, task app.Task) error {
	if err := validateTask(task); err != nil {
		return err
	}
	task = normalizeTask(task)
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO tasks (
            id, source_display, output_dir, output_name, format, status,
            progress, bytes_done, duration_ms, error_code, error_message,
            created_at, started_at, finished_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		task.ID,
		task.SourceDisplay,
		task.OutputDir,
		task.OutputName,
		task.Format,
		task.Status,
		task.Progress,
		task.BytesDone,
		nullInt64(task.DurationMS),
		nullString(task.ErrorCode),
		nullString(task.ErrorMessage),
		encodeTime(task.CreatedAt),
		nullTime(task.StartedAt),
		nullTime(task.FinishedAt),
		encodeTime(task.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("create task %q: %w", task.ID, err)
	}
	return nil
}

// Update replaces the persisted state for an existing task. It performs one
// atomic SQL statement and returns ErrNotFound when the ID is unknown.
func (s *Store) Update(ctx context.Context, task app.Task) error {
	if err := validateTask(task); err != nil {
		return err
	}
	task = normalizeTask(task)
	result, err := s.db.ExecContext(ctx, `
        UPDATE tasks SET
            source_display = ?, output_dir = ?, output_name = ?, format = ?,
            status = ?, progress = ?, bytes_done = ?, duration_ms = ?,
            error_code = ?, error_message = ?, created_at = ?, started_at = ?,
            finished_at = ?, updated_at = ?
        WHERE id = ?
    `,
		task.SourceDisplay,
		task.OutputDir,
		task.OutputName,
		task.Format,
		task.Status,
		task.Progress,
		task.BytesDone,
		nullInt64(task.DurationMS),
		nullString(task.ErrorCode),
		nullString(task.ErrorMessage),
		encodeTime(task.CreatedAt),
		nullTime(task.StartedAt),
		nullTime(task.FinishedAt),
		encodeTime(task.UpdatedAt),
		task.ID,
	)
	if err != nil {
		return fmt.Errorf("update task %q: %w", task.ID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check updated task %q: %w", task.ID, err)
	}
	if count == 0 {
		return fmt.Errorf("update task %q: %w", task.ID, ErrNotFound)
	}
	return nil
}

// Get returns a task by ID.
func (s *Store) Get(ctx context.Context, id string) (app.Task, error) {
	if strings.TrimSpace(id) == "" {
		return app.Task{}, fmt.Errorf("get task: %w", ErrNotFound)
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return app.Task{}, fmt.Errorf("get task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return app.Task{}, fmt.Errorf("get task %q: %w", id, err)
	}
	return task, nil
}

// List returns task history in reverse creation order. A zero limit means no
// limit; a positive limit restricts the number of returned rows.
func (s *Store) List(ctx context.Context, filter app.TaskFilter) ([]app.Task, error) {
	if filter.Status != "" && !filter.Status.Valid() {
		return nil, fmt.Errorf("list tasks: invalid status %q", filter.Status)
	}
	if filter.Limit < 0 {
		return nil, errors.New("list tasks: limit must be non-negative")
	}

	query := `SELECT ` + taskColumns + ` FROM tasks`
	args := make([]any, 0, 2)
	if filter.Status != "" {
		query += ` WHERE status = ?`
		args = append(args, filter.Status)
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	tasks := make([]app.Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return tasks, nil
}

// MarkInterrupted marks all queued and running tasks as interrupted. The
// update and its timestamp are one atomic statement. The returned count is the
// number of tasks changed.
func (s *Store) MarkInterrupted(ctx context.Context) (int, error) {
	now := encodeTime(time.Now())
	result, err := s.db.ExecContext(ctx, `
        UPDATE tasks
        SET status = 'interrupted', finished_at = ?, updated_at = ?
        WHERE status IN ('queued', 'running')
    `, now, now)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted tasks: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count interrupted tasks: %w", err)
	}
	return int(count), nil
}

func validateTask(task app.Task) error {
	if strings.TrimSpace(task.ID) == "" {
		return errors.New("task id is required")
	}
	if !task.Status.Valid() {
		return fmt.Errorf("invalid task status %q", task.Status)
	}
	return nil
}

func normalizeTask(task app.Task) app.Task {
	task.ID = strings.TrimSpace(task.ID)
	task.SourceDisplay = sanitizeSourceDisplay(task.SourceDisplay)
	now := time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
	}
	task.CreatedAt = task.CreatedAt.UTC()
	task.UpdatedAt = task.UpdatedAt.UTC()
	if task.StartedAt != nil {
		value := task.StartedAt.UTC()
		task.StartedAt = &value
	}
	if task.FinishedAt != nil {
		value := task.FinishedAt.UTC()
		task.FinishedAt = &value
	}
	return task
}

// sanitizeSourceDisplay is a defense-in-depth guard for callers that pass an
// input URL instead of a pre-sanitized display value. Non-URL labels are left
// intact because a display value is allowed to be user-friendly text.
func sanitizeSourceDisplay(value string) string {
	value = strings.TrimSpace(value)
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return value
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

func encodeTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return encodeTime(*value)
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (app.Task, error) {
	var (
		task                    app.Task
		status                  string
		durationMS              sql.NullInt64
		errorCode, errorMessage sql.NullString
		createdAt, updatedAt    string
		startedAt, finishedAt   sql.NullString
	)
	if err := row.Scan(
		&task.ID,
		&task.SourceDisplay,
		&task.OutputDir,
		&task.OutputName,
		&task.Format,
		&status,
		&task.Progress,
		&task.BytesDone,
		&durationMS,
		&errorCode,
		&errorMessage,
		&createdAt,
		&startedAt,
		&finishedAt,
		&updatedAt,
	); err != nil {
		return app.Task{}, err
	}
	task.Status = app.TaskStatus(status)
	if durationMS.Valid {
		value := durationMS.Int64
		task.DurationMS = &value
	}
	task.ErrorCode = errorCode.String
	task.ErrorMessage = errorMessage.String
	var err error
	if task.CreatedAt, err = decodeTime(createdAt); err != nil {
		return app.Task{}, fmt.Errorf("decode created_at: %w", err)
	}
	if task.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return app.Task{}, fmt.Errorf("decode updated_at: %w", err)
	}
	if task.StartedAt, err = decodeNullableTime(startedAt); err != nil {
		return app.Task{}, fmt.Errorf("decode started_at: %w", err)
	}
	if task.FinishedAt, err = decodeNullableTime(finishedAt); err != nil {
		return app.Task{}, fmt.Errorf("decode finished_at: %w", err)
	}
	return task, nil
}

func decodeTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	return time.Parse(time.RFC3339Nano, value)
}

func decodeNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := decodeTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func isMemoryPath(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:")
}

func isURIPath(path string) bool {
	return strings.HasPrefix(path, "file:")
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 1
	}
	if version != schemaVersion {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	return nil
}
