// Package app defines the narrow application-facing types shared by the CLI,
// HTTP API, and later task/media implementations.
package app

import (
	"context"
	"time"
)

// Version is the build-time application version. Release builds may override
// it with -ldflags -X.
var Version = "0.1.0-dev"

type HealthResponse struct {
	Status    string `json:"status"`
	Service   string `json:"service"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
}

// TaskStatus is intentionally a string so it remains stable in JSON and the
// SQLite representation used by later phases.
type TaskStatus string

const (
	TaskQueued      TaskStatus = "queued"
	TaskRunning     TaskStatus = "running"
	TaskSucceeded   TaskStatus = "succeeded"
	TaskFailed      TaskStatus = "failed"
	TaskCanceled    TaskStatus = "canceled"
	TaskInterrupted TaskStatus = "interrupted"
)

func (s TaskStatus) Valid() bool {
	switch s {
	case TaskQueued, TaskRunning, TaskSucceeded, TaskFailed, TaskCanceled, TaskInterrupted:
		return true
	default:
		return false
	}
}

// InspectRequest contains credentials only for the lifetime of a request.
// Implementations must not persist or include these fields in responses.
type InspectRequest struct {
	URL       string `json:"url"`
	Referer   string `json:"referer,omitempty"`
	Origin    string `json:"origin,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
	Cookie    string `json:"cookie,omitempty"`
}

type MediaInfo struct {
	SourceDisplay string        `json:"source_display"`
	Format        string        `json:"format,omitempty"`
	DurationMS    *int64        `json:"duration_ms,omitempty"`
	SizeBytes     *int64        `json:"size_bytes,omitempty"`
	Streams       []MediaStream `json:"streams,omitempty"`
	HLS           *HLSInfo      `json:"hls,omitempty"`
}

// HLSInfo is a bounded, credential-free summary of an HLS playlist. It is
// intentionally metadata-only: playlist, segment, key, and variant query
// strings are never returned to callers.
type HLSInfo struct {
	PlaylistType      string       `json:"playlist_type"`
	Availability      string       `json:"availability"`
	Variants          []HLSVariant `json:"variants,omitempty"`
	EncryptionMethods []string     `json:"encryption_methods,omitempty"`
	SegmentFormat     string       `json:"segment_format"`
	ByteRange         bool         `json:"byte_range"`
	SegmentCount      int          `json:"segment_count"`
}

type HLSVariant struct {
	Index            int    `json:"index"`
	URLDisplay       string `json:"url_display,omitempty"`
	Bandwidth        int64  `json:"bandwidth,omitempty"`
	AverageBandwidth int64  `json:"average_bandwidth,omitempty"`
	Width            int    `json:"width,omitempty"`
	Height           int    `json:"height,omitempty"`
	Codecs           string `json:"codecs,omitempty"`
}

type MediaStream struct {
	Index    int    `json:"index"`
	Kind     string `json:"kind"`
	Codec    string `json:"codec,omitempty"`
	Language string `json:"language,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Channels int    `json:"channels,omitempty"`
}

// CreateTaskRequest contains user-selected output settings and transient
// request headers. Only the non-sensitive fields belong in task history.
type CreateTaskRequest struct {
	URL             string `json:"url"`
	Referer         string `json:"referer,omitempty"`
	Origin          string `json:"origin,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	Cookie          string `json:"cookie,omitempty"`
	OutputDir       string `json:"output_dir,omitempty"`
	OutputName      string `json:"output_name,omitempty"`
	Format          string `json:"format,omitempty"`
	HLSVariantIndex *int   `json:"hls_variant_index,omitempty"`
}

type TaskFilter struct {
	Status TaskStatus
	Limit  int
}

type Task struct {
	ID            string     `json:"id"`
	SourceDisplay string     `json:"source_display"`
	OutputDir     string     `json:"output_dir"`
	OutputName    string     `json:"output_name"`
	Format        string     `json:"format"`
	Status        TaskStatus `json:"status"`
	Progress      float64    `json:"progress"`
	BytesDone     int64      `json:"bytes_done"`
	DurationMS    *int64     `json:"duration_ms,omitempty"`
	ErrorCode     string     `json:"error_code,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Service is the intentionally small seam used by both command and HTTP
// adapters. Phase 0 supplies an unavailable implementation; later phases can
// implement these methods with task/store/media packages without changing the
// adapters.
type Service interface {
	Inspect(context.Context, InspectRequest) (MediaInfo, error)
	CreateTask(context.Context, CreateTaskRequest) (Task, error)
	ListTasks(context.Context, TaskFilter) ([]Task, error)
	GetTask(context.Context, string) (Task, error)
	CancelTask(context.Context, string) (Task, error)
}
