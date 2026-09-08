// Package tasks owns the in-process download queue and task lifecycle.
//
// Request URLs and headers deliberately live only in queued or running work
// items. The store receives app.Task values, which contain only safe display
// metadata and execution results.
package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/media"
	"github.com/dzaneyo/riflo/internal/store"
)

const (
	defaultConcurrency            = 2
	progressPersistInterval       = 250 * time.Millisecond
	progressFractionDelta         = 0.02
	progressBytesDelta      int64 = 1 << 20
	maxErrorMessage               = 512
	maxHeaderValue                = 64 * 1024
	maxTaskIDRandomBytes          = 6
	terminalPersistAttempts       = 3
	terminalPersistRetryDelay     = 100 * time.Millisecond
)

var (
	// The URL matcher is used only as a final defence when a child process or a
	// test runner returns an arbitrary error string. The request-specific
	// replacement in redactError runs first, so this cannot expose a signed URL
	// query accidentally copied into an error.
	errorURLPattern         = regexp.MustCompile(`https?://[^\s"'<>]+`)
	secretQueryPattern      = regexp.MustCompile(`(?i)([?&](?:token|sig|signature|auth|authorization|key|expires|expiry|hash|hmac|session)(?:=|%3d))[^&\s]+`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b((?:token|sig|signature|auth|authorization|key|expires|expiry|hash|hmac|session)=)[^&\s]+`)
)

// Config configures a task service. Runner is a concrete media.Runner in the
// public API; tests in this package can use newWithRunner to install a narrow
// fake runner without widening the application contract.
type Config struct {
	Store            *store.Store
	Runner           *media.Runner
	DefaultOutputDir string
	Concurrency      int
}

// mediaRunner is the only media seam needed by this package. The real
// *media.Runner implements it directly.
type mediaRunner interface {
	Inspect(context.Context, app.InspectRequest) (app.MediaInfo, error)
	Download(context.Context, string, app.CreateTaskRequest, func(media.Progress)) (media.Result, error)
}

// Service implements app.Service and manages a bounded set of media workers.
type Service struct {
	store            *store.Store
	runner           mediaRunner
	defaultOutputDir string
	concurrency      int

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	closed  bool
	queue   []*queuedTask
	queued  map[string]*queuedTask
	active  map[string]context.CancelFunc
	workers sync.WaitGroup

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

var _ app.Service = (*Service)(nil)

type queuedTask struct {
	task app.Task
	req  app.CreateTaskRequest
}

// New initializes the service, marks tasks left by a prior process as
// interrupted, and starts the configured number of workers. A zero or
// negative concurrency uses the deliberately small default of two workers.
func New(ctx context.Context, cfg Config) (*Service, error) {
	return newWithRunner(ctx, cfg, cfg.Runner)
}

// newWithRunner is intentionally private. It lets package tests provide a
// deterministic fake while keeping the public Config tied to media.Runner.
func newWithRunner(ctx context.Context, cfg Config, runner mediaRunner) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("task store is required")
	}
	if runner == nil {
		return nil, errors.New("media runner is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := cfg.Store.MarkInterrupted(ctx); err != nil {
		return nil, fmt.Errorf("initialize task service: %w", err)
	}

	defaultDir, err := normalizeOutputDir(cfg.DefaultOutputDir)
	if err != nil {
		return nil, app.Invalid("invalid default output directory")
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	serviceCtx, cancel := context.WithCancel(ctx)
	s := &Service{
		store:            cfg.Store,
		runner:           runner,
		defaultOutputDir: defaultDir,
		concurrency:      concurrency,
		ctx:              serviceCtx,
		cancel:           cancel,
		queued:           make(map[string]*queuedTask),
		active:           make(map[string]context.CancelFunc),
		closeDone:        make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	for i := 0; i < s.concurrency; i++ {
		s.workers.Add(1)
		go s.worker()
	}
	return s, nil
}

// Close rejects new work, cancels active media processes, marks work that was
// still queued as canceled, releases all transient request data, and waits
// for every worker to exit. It is safe to call more than once.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.shutdown()
		close(s.closeDone)
	})
	<-s.closeDone
	return s.closeErr
}

func (s *Service) shutdown() error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	pending := s.queue
	s.queue = nil
	s.queued = make(map[string]*queuedTask)
	activeCancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		activeCancels = append(activeCancels, cancel)
	}
	s.cond.Broadcast()
	s.mu.Unlock()

	for _, cancel := range activeCancels {
		cancel()
	}

	var firstErr error
	// Clear request fields before doing any database work. This makes the
	// lifetime guarantee independent of a slow SQLite call during shutdown.
	for _, item := range pending {
		item.req = app.CreateTaskRequest{}
		item.task.Status = app.TaskCanceled
		item.task.ErrorCode = "canceled"
		item.task.ErrorMessage = "task canceled"
		finish := time.Now().UTC()
		item.task.FinishedAt = &finish
		item.task.UpdatedAt = finish
		if err := s.store.Update(context.Background(), item.task); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cancel queued task %q during close: %w", item.task.ID, err)
		}
	}
	s.workers.Wait()
	return firstErr
}

func (s *Service) worker() {
	defer s.workers.Done()
	for {
		s.mu.Lock()
		for !s.closed && len(s.queue) == 0 {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		item := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		// A canceled queued task is removed from queued before it reaches this
		// point. Keep the map check as a race-proof guard for stale pointers.
		if _, ok := s.queued[item.task.ID]; !ok {
			s.mu.Unlock()
			continue
		}
		delete(s.queued, item.task.ID)

		runCtx, runCancel := context.WithCancel(s.ctx)
		s.active[item.task.ID] = runCancel
		started := time.Now().UTC()
		item.task.Status = app.TaskRunning
		item.task.StartedAt = &started
		item.task.UpdatedAt = started
		// Serializing this state transition with CancelTask prevents a cancel
		// request from observing a half-started task.
		updateErr := s.store.Update(context.Background(), item.task)
		s.mu.Unlock()

		if updateErr != nil {
			s.finishFailed(item.task, updateErr, item.req, runCancel)
			item.req = app.CreateTaskRequest{}
			continue
		}
		s.execute(item, runCtx, runCancel)
	}
}

// Inspect probes media and returns only a sanitized summary. It does not
// retain the request after the call returns.
func (s *Service) Inspect(ctx context.Context, req app.InspectRequest) (app.MediaInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateURL(req.URL); err != nil {
		return app.MediaInfo{}, err
	}
	if err := validateHeaders(req.Referer, req.Origin, req.UserAgent, req.Cookie); err != nil {
		return app.MediaInfo{}, err
	}
	if err := validateOrigin(req.Origin); err != nil {
		return app.MediaInfo{}, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return app.MediaInfo{}, app.NewError("service_closed", "task service is closed", 503)
	}
	info, err := s.runner.Inspect(ctx, req)
	if err != nil {
		return app.MediaInfo{}, mapMediaError("inspect", err, req.URL, req.Referer, req.Origin, req.UserAgent, req.Cookie)
	}
	// The request is the source of truth for the display value. This avoids
	// trusting an arbitrary runner implementation to echo a credential-bearing
	// URL in the API response.
	_, info.SourceDisplay, _ = normalizedURL(req.URL)
	return info, nil
}

// CreateTask validates and queues a download. The complete request is held
// only by the in-memory queued/running work item and is cleared on completion,
// cancellation, or service shutdown.
func (s *Service) CreateTask(ctx context.Context, req app.CreateTaskRequest) (app.Task, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return app.Task{}, app.NewError("canceled", "operation canceled", 408)
	}
	fullURL, sourceDisplay, err := normalizedURL(req.URL)
	if err != nil {
		return app.Task{}, err
	}
	if err := validateHeaders(req.Referer, req.Origin, req.UserAgent, req.Cookie); err != nil {
		return app.Task{}, err
	}
	if err := validateOrigin(req.Origin); err != nil {
		return app.Task{}, err
	}
	format, err := normalizeFormat(req.Format)
	if err != nil {
		return app.Task{}, err
	}
	if req.HLSVariantIndex != nil && *req.HLSVariantIndex < 0 {
		return app.Task{}, app.Invalid("hls variant index must be non-negative")
	}
	outputDir, err := normalizeOutputDir(req.OutputDir)
	if err != nil {
		return app.Task{}, app.Invalid("invalid output directory")
	}
	if req.OutputDir == "" || strings.TrimSpace(req.OutputDir) == "" {
		outputDir = s.defaultOutputDir
	}
	outputName, err := normalizeOutputName(req.OutputName, format, "")
	if err != nil {
		return app.Task{}, err
	}

	// Generate the ID before deriving the default name. crypto/rand failure is
	// surfaced instead of silently falling back to a predictable identifier.
	id, err := newTaskID()
	if err != nil {
		return app.Task{}, app.NewError("internal_error", "could not create task id", 500)
	}
	if strings.TrimSpace(req.OutputName) == "" {
		outputName = "download-" + shortTaskID(id) + "." + format
	}
	req.URL = fullURL
	req.OutputDir = outputDir
	req.OutputName = outputName
	req.Format = format

	now := time.Now().UTC()
	task := app.Task{
		ID:            id,
		SourceDisplay: sourceDisplay,
		OutputDir:     outputDir,
		OutputName:    outputName,
		Format:        format,
		Status:        app.TaskQueued,
		Progress:      0,
		BytesDone:     0,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	item := &queuedTask{task: task, req: req}

	// Keep the closed check, durable insert, and queue append under one mutex.
	// This prevents Close from leaving a successfully inserted task without a
	// corresponding in-memory request.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		item.req = app.CreateTaskRequest{}
		return app.Task{}, app.NewError("service_closed", "task service is closed", 503)
	}
	if err := s.store.Create(context.Background(), task); err != nil {
		item.req = app.CreateTaskRequest{}
		return app.Task{}, mapStoreError("create task", err)
	}
	s.queue = append(s.queue, item)
	s.queued[id] = item
	s.cond.Signal()
	return task, nil
}

func (s *Service) execute(item *queuedTask, runCtx context.Context, runCancel context.CancelFunc) {
	defer runCancel()
	defer func() { item.req = app.CreateTaskRequest{} }()

	task := item.task
	progress := taskProgress{task: task}
	result, downloadErr := s.runner.Download(runCtx, task.ID, item.req, func(snapshot media.Progress) {
		progress.apply(snapshot)
		s.persistProgress(&progress, false)
	})

	if isCancellation(downloadErr, runCtx) {
		progress.task.Status = app.TaskCanceled
		progress.task.ErrorCode = "canceled"
		progress.task.ErrorMessage = "task canceled"
	} else if downloadErr == nil {
		progress.task.Status = app.TaskSucceeded
		progress.task.Progress = 1
		if result.DurationMS > 0 {
			value := result.DurationMS
			progress.task.DurationMS = &value
		}
		progress.task.ErrorCode = ""
		progress.task.ErrorMessage = ""
	} else {
		mapped := mapMediaError("download", downloadErr, item.req.URL, item.req.Referer, item.req.Origin, item.req.UserAgent, item.req.Cookie)
		progress.task.Status = app.TaskFailed
		progress.task.ErrorCode = mapped.Code
		progress.task.ErrorMessage = boundedMessage(mapped.Message)
	}
	finish := time.Now().UTC()
	progress.task.FinishedAt = &finish
	progress.task.UpdatedAt = finish
	s.persistTerminal(&progress)

	s.mu.Lock()
	delete(s.active, task.ID)
	s.mu.Unlock()
}

func (s *Service) finishFailed(task app.Task, err error, req app.CreateTaskRequest, runCancel context.CancelFunc) {
	defer runCancel()
	task.Status = app.TaskFailed
	mapped := mapStoreError("run task", err)
	task.ErrorCode = mapped.Code
	task.ErrorMessage = boundedMessage(redactError(mapped.Message, req.URL, req.Referer, req.Origin, req.UserAgent, req.Cookie))
	finish := time.Now().UTC()
	task.FinishedAt = &finish
	task.UpdatedAt = finish
	_ = s.store.Update(context.Background(), task)
	s.mu.Lock()
	delete(s.active, task.ID)
	s.mu.Unlock()
	// A failed state transition must not retain the request fields.
	req = app.CreateTaskRequest{}
}

type taskProgress struct {
	task          app.Task
	lastPersisted time.Time
	lastFraction  float64
	lastBytes     int64
	persisted     bool
}

func (p *taskProgress) apply(snapshot media.Progress) {
	if snapshot.BytesDone >= 0 {
		p.task.BytesDone = snapshot.BytesDone
	}
	if snapshot.DurationMS > 0 {
		value := snapshot.DurationMS
		p.task.DurationMS = &value
	}
	if snapshot.Fraction >= 0 && !math.IsNaN(snapshot.Fraction) && !math.IsInf(snapshot.Fraction, 0) {
		p.task.Progress = clamp(snapshot.Fraction, 0, 1)
	}
}

func (s *Service) persistProgress(p *taskProgress, force bool) {
	now := time.Now().UTC()
	if !force && p.persisted && now.Sub(p.lastPersisted) < progressPersistInterval &&
		math.Abs(p.task.Progress-p.lastFraction) < progressFractionDelta &&
		absInt64(p.task.BytesDone-p.lastBytes) < progressBytesDelta {
		return
	}
	p.task.UpdatedAt = now
	if err := s.store.Update(context.Background(), p.task); err != nil {
		// There is no useful recovery path from a closed/broken local store while
		// media is running. The worker still completes and the final state is
		// attempted, keeping process and goroutine ownership deterministic.
		return
	}
	p.lastPersisted = now
	p.lastFraction = p.task.Progress
	p.lastBytes = p.task.BytesDone
	p.persisted = true
}

func (s *Service) persistTerminal(p *taskProgress) {
	for attempt := 0; attempt < terminalPersistAttempts; attempt++ {
		p.task.UpdatedAt = time.Now().UTC()
		if err := s.store.Update(context.Background(), p.task); err == nil {
			p.lastPersisted = p.task.UpdatedAt
			p.lastFraction = p.task.Progress
			p.lastBytes = p.task.BytesDone
			p.persisted = true
			return
		}
		if attempt == terminalPersistAttempts-1 {
			return
		}
		time.Sleep(terminalPersistRetryDelay * time.Duration(attempt+1))
	}
}

// ListTasks reads durable history. No in-memory request information is
// merged into the result.
func (s *Service) ListTasks(ctx context.Context, filter app.TaskFilter) ([]app.Task, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tasks, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, mapStoreError("list tasks", err)
	}
	return tasks, nil
}

// GetTask reads one task from durable history.
func (s *Service) GetTask(ctx context.Context, id string) (app.Task, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	task, err := s.store.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return app.Task{}, mapStoreError("get task", err)
	}
	return task, nil
}

// CancelTask requests cancellation. Queued work is made terminal immediately;
// running work returns its current durable snapshot while its process exits.
func (s *Service) CancelTask(ctx context.Context, id string) (app.Task, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return app.Task{}, app.NewError("not_found", "task not found", 404)
	}

	s.mu.Lock()
	if item, ok := s.queued[id]; ok {
		delete(s.queued, id)
		s.removeQueuedLocked(id)
		item.req = app.CreateTaskRequest{}
		task := item.task
		now := time.Now().UTC()
		task.Status = app.TaskCanceled
		task.ErrorCode = "canceled"
		task.ErrorMessage = "task canceled"
		task.FinishedAt = &now
		task.UpdatedAt = now
		s.mu.Unlock()
		if err := s.store.Update(context.Background(), task); err != nil {
			return app.Task{}, mapStoreError("cancel task", err)
		}
		return task, nil
	}
	if cancel, ok := s.active[id]; ok {
		cancel()
		s.mu.Unlock()
		// The runner owns process shutdown. Returning its current snapshot avoids
		// blocking an HTTP request on a slow child process; the final worker write
		// settles canceled/failed/succeeded deterministically.
		return s.GetTask(ctx, id)
	}
	s.mu.Unlock()

	task, err := s.store.Get(ctx, id)
	if err != nil {
		return app.Task{}, mapStoreError("cancel task", err)
	}
	if isTerminal(task.Status) {
		return task, nil
	}
	// A task can be observed between a worker's durable transition and active
	// map insertion only while holding the service mutex, so this branch is a
	// stale/external-store case. Do not overwrite another process's work.
	return task, nil
}

func (s *Service) removeQueuedLocked(id string) {
	for i, item := range s.queue {
		if item != nil && item.task.ID == id {
			item.req = app.CreateTaskRequest{}
			copy(s.queue[i:], s.queue[i+1:])
			s.queue[len(s.queue)-1] = nil
			s.queue = s.queue[:len(s.queue)-1]
			return
		}
	}
}

func isTerminal(status app.TaskStatus) bool {
	switch status {
	case app.TaskSucceeded, app.TaskFailed, app.TaskCanceled, app.TaskInterrupted:
		return true
	default:
		return false
	}
}

func isCancellation(err error, ctx context.Context) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil)
}

func newTaskID() (string, error) {
	var random [maxTaskIDRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "task-" + hex.EncodeToString(random[:]), nil
}

func shortTaskID(id string) string {
	id = strings.TrimPrefix(id, "task-")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func normalizeFormat(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "mp4"
	}
	if value != "mp4" && value != "mkv" {
		return "", app.Invalid("format must be mp4 or mkv")
	}
	return value, nil
}

func normalizedURL(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if err := validateURL(value); err != nil {
		return "", "", err
	}
	parsed, _ := url.Parse(value)
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return value, parsed.String(), nil
}

func validateURL(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return app.Invalid("url is required")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return app.Invalid("url contains invalid control characters")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return app.Invalid("url must use http or https")
	}
	return nil
}

func validateHeaders(values ...string) error {
	for _, value := range values {
		if len(value) > maxHeaderValue || strings.ContainsAny(value, "\r\n\x00") {
			return app.Invalid("request header is invalid")
		}
	}
	return nil
}

func validateOrigin(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return app.Invalid("origin must be an HTTP origin")
	}
	return nil
}

func normalizeOutputDir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "."
	}
	if strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("output directory contains invalid control characters")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		return "", errors.New("output directory is not a directory")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	return filepath.Clean(abs), nil
}

func normalizeOutputName(value, format, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if value == "" {
		return "", nil
	}
	if value == "." || value == ".." || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "\r\n") || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) || filepath.IsAbs(value) || strings.HasPrefix(value, "-") {
		return "", app.Invalid("output name must be a safe file name")
	}
	if ext := filepath.Ext(value); ext != "" && !strings.EqualFold(ext, "."+format) {
		return "", app.Invalid("output name extension must match format")
	}
	if filepath.Ext(value) == "" {
		value += "." + format
	}
	return value, nil
}

func mapStoreError(operation string, err error) *app.Error {
	if errors.Is(err, store.ErrNotFound) {
		return app.NewError("not_found", "task not found", 404)
	}
	return app.NewError("internal_error", boundedMessage(operation+" failed"), 500)
}

func mapMediaError(operation string, err error, secrets ...string) *app.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, media.ErrInvalidURL) || errors.Is(err, media.ErrUnsupportedFormat) {
		return app.Invalid("invalid media request")
	}
	if errors.Is(err, media.ErrOutputExists) {
		return app.NewError("output_exists", "output file already exists", 409)
	}
	if errors.Is(err, media.ErrToolUnavailable) {
		return app.NewError("media_tool_unavailable", "ffmpeg or ffprobe is unavailable", 503)
	}
	if errors.Is(err, media.ErrAccessDenied) {
		return app.NewError("media_access_denied", "media server denied access; try adding Referer or Cookie", 502)
	}
	if errors.Is(err, media.ErrRateLimited) {
		return app.NewError("media_rate_limited", "media server is rate limited; try again later", 429)
	}
	if errors.Is(err, media.ErrTimeout) {
		return app.NewError("media_timeout", "media request timed out", 504)
	}
	if errors.Is(err, media.ErrNetwork) {
		return app.NewError("media_network_error", "media network request failed", 502)
	}
	if errors.Is(err, media.ErrHTTPStatus) {
		return app.NewError("media_http_error", "media server returned an HTTP error", 502)
	}
	if errors.Is(err, media.ErrInvalidPlaylist) {
		return app.NewError("hls_invalid_playlist", "media playlist is invalid", 422)
	}
	if errors.Is(err, media.ErrVariantUnavailable) {
		return app.NewError("hls_variant_unavailable", "the selected HLS quality is unavailable", 422)
	}
	if errors.Is(err, media.ErrURLExpired) {
		return app.NewError("media_url_expired", "the media URL is unavailable or may have expired", 410)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return app.NewError("canceled", "operation canceled", 408)
	}
	message := redactError(err.Error(), secrets...)
	if message == "" {
		message = operation + " failed"
	}
	return app.NewError(operation+"_failed", boundedMessage(message), 502)
}

func sanitizeSourceDisplay(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func redactError(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
			if parsed, err := url.Parse(secret); err == nil && parsed.RawQuery != "" {
				// A child may print only the query component instead of the
				// complete URL. Redact both the complete raw query and each value
				// in it, while retaining no credential-bearing material.
				message = strings.ReplaceAll(message, parsed.RawQuery, "[redacted query]")
				for key, values := range parsed.Query() {
					for _, value := range values {
						if value != "" {
							message = strings.ReplaceAll(message, value, "[redacted]")
						}
					}
					message = strings.ReplaceAll(message, key+"=", key+"=[redacted]")
				}
			}
		}
	}
	// Remove query and fragment from any URL-shaped text, including a URL that
	// was rebuilt by a child process and no longer exactly matches the input.
	message = errorURLPattern.ReplaceAllStringFunc(message, func(value string) string {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return "[redacted url]"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
	message = secretQueryPattern.ReplaceAllString(message, "$1[redacted]")
	message = secretAssignmentPattern.ReplaceAllString(message, "$1[redacted]")
	message = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || (r < utf8.RuneSelf && r < 0x20) {
			return ' '
		}
		return r
	}, message)
	return boundedMessage(strings.TrimSpace(message))
}

func boundedMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxErrorMessage {
		return value
	}
	limit := maxErrorMessage - len("...")
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + "..."
}

func clamp(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
