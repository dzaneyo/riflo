// Package media contains the small ffprobe/ffmpeg process boundary used by
// riflo.  It deliberately passes every value as an exec argument and keeps
// request credentials in memory for the lifetime of one operation only.
package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/ffmpegcap"
)

const (
	// UnknownFraction is used when the input does not expose a usable duration.
	// BytesDone, DurationMS, and OutTimeMS use zero for an unknown value.
	UnknownFraction = -1.0

	maxHeaderValue          = 64 * 1024
	maxStderrBytes          = 8 * 1024
	maxErrorBytes           = 4 * 1024
	maxHLSBodyBytes         = 2 * 1024 * 1024
	hlsProbeTimeout         = 8 * time.Second
	maxHLSRedirects         = 5
	hlsRequestAttempts      = 3
	hlsRetryBaseDelay       = 200 * time.Millisecond
	ffmpegReconnectRetries  = 3
	ffmpegReconnectDelayMax = 2
	ffmpegReconnectTotalMax = 10
)

var (
	// ErrOutputExists is returned before, or while, publishing a completed
	// download when the requested destination already exists.
	ErrOutputExists = errors.New("output file already exists")
	// ErrInvalidURL is returned for non-HTTP(S) media URLs.
	ErrInvalidURL = errors.New("url must use http or https")
	// ErrUnsupportedFormat is returned for formats other than mp4 and mkv.
	ErrUnsupportedFormat = errors.New("format must be mp4 or mkv")
	// ErrToolUnavailable identifies a missing or non-executable media tool.
	ErrToolUnavailable = errors.New("media tool is unavailable")
	// ErrAccessDenied is returned when the upstream media server responds with
	// an authentication/authorization failure. The error intentionally carries
	// no URL, header, or response body.
	ErrAccessDenied = errors.New("media access denied")
	// ErrHTTPStatus identifies a bounded HLS preflight response with an
	// unsuccessful status other than 401/403.
	ErrHTTPStatus = errors.New("media request returned an unsuccessful status")
	// ErrRateLimited identifies an upstream 429 response after bounded retries.
	ErrRateLimited = errors.New("media request was rate limited")
	// ErrTimeout identifies a bounded network or media-tool timeout.
	ErrTimeout = errors.New("media request timed out")
	// ErrNetwork identifies a network failure that is not an HTTP status.
	ErrNetwork = errors.New("media network request failed")
	// ErrInvalidPlaylist identifies a response that cannot be used as HLS.
	ErrInvalidPlaylist = errors.New("HLS playlist is invalid")
	// ErrVariantUnavailable identifies a selected HLS quality that is not
	// present in the current master playlist.
	ErrVariantUnavailable = errors.New("selected HLS variant is unavailable")
	// ErrURLExpired identifies a media URL that is no longer available.
	ErrURLExpired = errors.New("media URL is unavailable or expired")
	// ErrPlaylistTooLarge prevents a preflight from reading an unbounded body.
	ErrPlaylistTooLarge = errors.New("HLS playlist is too large")
)

// Config controls the executable paths used by Runner. Empty paths resolve
// ffprobe and ffmpeg through PATH at execution time.
type Config struct {
	FFprobePath string
	FFmpegPath  string

	capOnce sync.Once
	caps    ffmpegcap.Capabilities
	capErr  error
}

// RunnerConfig is kept as a descriptive alias for callers that prefer the
// longer configuration name.
type RunnerConfig = Config

// Runner invokes ffprobe and ffmpeg without a shell.
type Runner struct {
	// The fields are exported so an application can replace a tool for tests or
	// package-local deployments after constructing a Runner. Empty values still
	// mean the corresponding executable name on PATH.
	FFprobePath string
	FFmpegPath  string
}

// NewRunner creates a media runner. Empty executable paths use the names
// "ffprobe" and "ffmpeg", allowing the normal process PATH lookup.
func NewRunner(ffprobePath, ffmpegPath string) *Runner {
	return &Runner{
		FFprobePath: defaultToolPath(ffprobePath, "ffprobe"),
		FFmpegPath:  defaultToolPath(ffmpegPath, "ffmpeg"),
	}
}

// NewWithConfig constructs a runner from the named configuration type.
func NewWithConfig(cfg Config) *Runner { return NewRunner(cfg.FFprobePath, cfg.FFmpegPath) }

// New is a short configuration-based constructor for callers that prefer a
// struct over positional executable paths.
func New(cfg Config) *Runner { return NewWithConfig(cfg) }

// DefaultRunner uses ffprobe and ffmpeg from PATH.
func DefaultRunner() *Runner { return NewRunner("", "") }

func defaultToolPath(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

// Progress is a machine-friendly snapshot emitted while ffmpeg runs.
// Fraction is in [0, 1] when DurationMS is known, and UnknownFraction when
// the input duration is unavailable. OutTimeMS is the processed media time;
// BytesDone is ffmpeg's total_size value when available.
type Progress struct {
	Fraction   float64 `json:"fraction"`
	BytesDone  int64   `json:"bytes_done"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	OutTimeMS  int64   `json:"out_time_ms,omitempty"`
}

// Result describes a successfully published output file. DurationMS is zero
// when ffmpeg did not report a processed media duration.
type Result struct {
	OutputPath string `json:"output_path"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Inspect runs ffprobe and maps its JSON output to the application media
// summary. Request credentials are passed only to the child process and are
// never copied into MediaInfo.
func (r *Runner) Inspect(ctx context.Context, req app.InspectRequest) (app.MediaInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := normalizeRequestWithOrigin(req.URL, req.Referer, req.Origin, req.UserAgent, req.Cookie)
	if err != nil {
		return app.MediaInfo{}, err
	}
	var hls *app.HLSInfo
	preflightAttempted := false
	if isLikelyHLSURL(request.URL) {
		preflightAttempted = true
		preflight, preflightErr := r.inspectHLS(ctx, request)
		if preflightErr != nil {
			if shouldReturnHLSProbeError(preflightErr, ctx) {
				return app.MediaInfo{}, preflightErr
			}
			// Preflight is an additive diagnostic. If the playlist endpoint is
			// unavailable or too large, let ffprobe make the final media decision.
		} else if preflight != nil {
			hls = preflight
		}
	}
	info, err := r.inspect(ctx, request)
	if err != nil {
		return app.MediaInfo{}, err
	}
	// Some players serve HLS from an extensionless endpoint and advertise it
	// only through Content-Type. ffprobe is the safe discriminator for that
	// case; only after it reports an HLS format do we issue the bounded playlist
	// request. This keeps ordinary large media off the preflight path.
	if hls == nil && !preflightAttempted && isHLSFormat(info.Format) {
		preflight, preflightErr := r.inspectHLS(ctx, request)
		if preflightErr != nil {
			if shouldReturnHLSProbeError(preflightErr, ctx) {
				return app.MediaInfo{}, preflightErr
			}
		} else if preflight != nil {
			hls = preflight
		}
	}
	info.HLS = hls
	return info, nil
}

func isHLSFormat(format string) bool {
	for _, value := range strings.Split(strings.ToLower(format), ",") {
		if strings.TrimSpace(value) == "hls" {
			return true
		}
	}
	return false
}

// Download runs ffmpeg into a same-directory temporary file, then publishes
// it without replacing an existing destination. A best-effort ffprobe pass is
// used to obtain a duration for progress; ffmpeg remains usable when probing
// is unavailable or cannot describe an otherwise downloadable input.
func (r *Runner) Download(ctx context.Context, taskID string, req app.CreateTaskRequest, report func(Progress)) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := normalizeRequestWithOrigin(req.URL, req.Referer, req.Origin, req.UserAgent, req.Cookie)
	if err != nil {
		return Result{}, err
	}
	format, err := normalizeFormat(req.Format)
	if err != nil {
		return Result{}, err
	}
	spec, err := prepareOutput(req.OutputDir, req.OutputName, request.URL, format)
	if err != nil {
		return Result{}, err
	}
	if err := contextErr(ctx); err != nil {
		return Result{}, err
	}

	// A selected HLS quality is resolved at download time, immediately before
	// ffprobe/ffmpeg. The signed child URL stays in this in-memory request only
	// and is never put into the task store or response model.
	isHLSInput := isLikelyHLSURL(request.URL)
	if req.HLSVariantIndex != nil {
		if *req.HLSVariantIndex < 0 {
			return Result{}, ErrVariantUnavailable
		}
		variantURL, resolveErr := r.resolveHLSVariant(ctx, request, *req.HLSVariantIndex)
		if resolveErr != nil {
			return Result{}, resolveErr
		}
		request.URL = variantURL
		isHLSInput = true
	}

	// Probing is intentionally best effort. Some valid ffmpeg inputs do not
	// yield useful ffprobe metadata, and a missing ffprobe must not make the
	// download process impossible. Cancellation is the one error propagated.
	var durationMS int64
	if info, probeErr := r.inspect(ctx, request); probeErr == nil {
		if info.DurationMS != nil && *info.DurationMS > 0 {
			durationMS = *info.DurationMS
		}
	} else if err := contextErr(ctx); err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(spec.dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create output directory: %w", err)
	}
	if err := ensureDestinationAbsent(spec.path); err != nil {
		return Result{}, err
	}

	temp, err := os.CreateTemp(spec.dir, tempPattern(taskID, format))
	if err != nil {
		return Result{}, fmt.Errorf("create temporary output: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return Result{}, fmt.Errorf("prepare temporary output: %w", err)
	}
	defer func() {
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()

	caps := r.capabilities(ctx)
	args, err := ffmpegArgs(request, spec, tempPath, format, isHLSInput, caps)
	if err != nil {
		return Result{}, err
	}
	state := progressState{durationMS: durationMS}
	if report != nil {
		report(state.snapshot())
	}
	if err := r.runFFmpeg(ctx, args, &state, report, request); err != nil {
		return Result{}, err
	}
	if err := contextErr(ctx); err != nil {
		return Result{}, err
	}

	if durationMS > 0 {
		state.fraction = 1
	}
	if report != nil {
		report(state.snapshot())
	}
	if err := publishNoReplace(tempPath, spec.path); err != nil {
		return Result{}, err
	}
	tempPath = ""

	resultDuration := state.outTimeMS
	if resultDuration == 0 {
		resultDuration = durationMS
	}
	return Result{OutputPath: spec.path, DurationMS: resultDuration}, nil
}

func isLikelyHLSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	return strings.HasSuffix(path, ".m3u8") || strings.Contains(path, ".m3u8/")
}

// inspectHLS performs a bounded playlist request for an explicit m3u8 URL.
// It is intentionally separate from ffprobe: ordinary non-HLS responses can
// still fall back to ffprobe, while actionable upstream failures retain a
// stable classification for the API layer.
func (r *Runner) inspectHLS(ctx context.Context, request mediaRequest) (*app.HLSInfo, error) {
	body, finalURL, err := r.fetchHLSPlaylist(ctx, request)
	if err != nil {
		return nil, err
	}
	info, err := parseHLSPlaylist(body, finalURL)
	if errors.Is(err, errNotHLSPlaylist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPlaylist, err)
	}
	return &info, nil
}

func (r *Runner) fetchHLSPlaylist(ctx context.Context, request mediaRequest) ([]byte, *url.URL, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, hlsProbeTimeout)
	defer cancel()
	initialURL, err := url.Parse(request.URL)
	if err != nil || initialURL == nil {
		return nil, nil, ErrInvalidURL
	}
	client := &http.Client{
		Timeout: hlsProbeTimeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= maxHLSRedirects {
				return fmt.Errorf("%w: too many HLS redirects", ErrNetwork)
			}
			if sameHTTPOrigin(initialURL, next.URL) {
				setTransientHeader(next.Header, "Referer", request.Referer)
				setTransientHeader(next.Header, "Origin", request.Origin)
				setTransientHeader(next.Header, "User-Agent", request.UserAgent)
				setTransientHeader(next.Header, "Cookie", request.Cookie)
				return nil
			}
			// A playlist CDN redirect may cross origins. Do not forward user
			// supplied Cookie/Referer values to a new origin. Origin is a
			// browser-style context header and is safe to carry transiently.
			next.Header.Del("Referer")
			next.Header.Del("Cookie")
			setTransientHeader(next.Header, "Origin", request.Origin)
			setTransientHeader(next.Header, "User-Agent", request.UserAgent)
			return nil
		},
	}

	var lastErr error
	for attempt := 0; attempt < hlsRequestAttempts; attempt++ {
		if err := contextErr(probeCtx); err != nil {
			return nil, nil, classifyHTTPError(err)
		}
		hlsRequest, requestErr := http.NewRequestWithContext(probeCtx, http.MethodGet, request.URL, nil)
		if requestErr != nil {
			return nil, nil, requestErr
		}
		hlsRequest.Header.Set("Accept", "application/vnd.apple.mpegurl, application/x-mpegURL, text/plain;q=0.8")
		setTransientHeader(hlsRequest.Header, "Referer", request.Referer)
		setTransientHeader(hlsRequest.Header, "Origin", request.Origin)
		setTransientHeader(hlsRequest.Header, "User-Agent", request.UserAgent)
		setTransientHeader(hlsRequest.Header, "Cookie", request.Cookie)

		response, requestErr := client.Do(hlsRequest)
		if requestErr != nil {
			lastErr = classifyHTTPError(requestErr)
			if !isRetryableHLSRequestError(lastErr) || attempt == hlsRequestAttempts-1 {
				return nil, nil, lastErr
			}
			if err := waitHLSRetry(probeCtx, attempt); err != nil {
				return nil, nil, classifyHTTPError(err)
			}
			continue
		}

		statusErr := hlsStatusError(response.StatusCode)
		if statusErr != nil {
			statusCode := response.StatusCode
			_ = response.Body.Close()
			lastErr = statusErr
			if !isRetryableHLSStatus(statusCode) || attempt == hlsRequestAttempts-1 {
				return nil, nil, lastErr
			}
			if err := waitHLSRetry(probeCtx, attempt); err != nil {
				return nil, nil, classifyHTTPError(err)
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxHLSBodyBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			lastErr = classifyHTTPError(readErr)
			if !isRetryableHLSRequestError(lastErr) || attempt == hlsRequestAttempts-1 {
				return nil, nil, lastErr
			}
			if err := waitHLSRetry(probeCtx, attempt); err != nil {
				return nil, nil, classifyHTTPError(err)
			}
			continue
		}
		if len(body) > maxHLSBodyBytes {
			return nil, nil, fmt.Errorf("%w: playlist exceeds size limit", ErrInvalidPlaylist)
		}
		finalURL := response.Request.URL
		if finalURL == nil {
			finalURL = initialURL
		}
		return body, finalURL, nil
	}
	return nil, nil, lastErr
}

func hlsStatusError(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: upstream access denied", ErrAccessDenied)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: upstream rate limit", ErrRateLimited)
	case status == http.StatusRequestTimeout:
		return fmt.Errorf("%w: upstream request timed out", ErrTimeout)
	case status == http.StatusNotFound || status == http.StatusGone:
		return fmt.Errorf("%w: upstream media URL is unavailable", ErrURLExpired)
	case status < http.StatusOK || status >= http.StatusMultipleChoices:
		return fmt.Errorf("%w: upstream returned an HTTP error", ErrHTTPStatus)
	default:
		return nil
	}
}

func classifyHTTPError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: request deadline exceeded", ErrTimeout)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: network timeout", ErrTimeout)
	}
	return fmt.Errorf("%w: request failed", ErrNetwork)
}

func isRetryableHLSRequestError(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrNetwork) || errors.Is(err, ErrTimeout)
}

func isRetryableHLSStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func waitHLSRetry(ctx context.Context, attempt int) error {
	delay := hlsRetryBaseDelay * time.Duration(1<<attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func shouldReturnHLSProbeError(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	// A response that is simply not HLS is represented by a nil summary and can
	// still fall back to ffprobe. Once the bounded HLS request has a stable
	// upstream response or playlist failure, preserve that classification.
	// Transport failures remain additive because ffprobe may use a compatible
	// protocol path or provide a more precise media-tool diagnosis.
	for _, stable := range []error{
		ErrAccessDenied,
		ErrRateLimited,
		ErrHTTPStatus,
		ErrInvalidPlaylist,
		ErrURLExpired,
	} {
		if errors.Is(err, stable) {
			return true
		}
	}
	return errors.Is(err, context.Canceled) || (errors.Is(err, context.DeadlineExceeded) && ctx != nil && ctx.Err() != nil)
}

func (r *Runner) resolveHLSVariant(ctx context.Context, request mediaRequest, index int) (string, error) {
	body, finalURL, err := r.fetchHLSPlaylist(ctx, request)
	if err != nil {
		return "", err
	}
	parsed, err := parseHLSPlaylistDetailed(body, finalURL)
	if errors.Is(err, errNotHLSPlaylist) {
		return "", ErrInvalidPlaylist
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPlaylist, err)
	}
	if parsed.Info.PlaylistType != hlsPlaylistTypeMaster {
		return "", ErrVariantUnavailable
	}
	for _, variant := range parsed.Variants {
		if variant.Index == index {
			return variant.URL, nil
		}
	}
	return "", ErrVariantUnavailable
}

func setTransientHeader(headers http.Header, name, value string) {
	if value == "" {
		headers.Del(name)
		return
	}
	headers.Set(name, value)
}

func sameHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

type probeDocument struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

type probeStream struct {
	Index     int               `json:"index"`
	CodecType string            `json:"codec_type"`
	CodecName string            `json:"codec_name"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Channels  int               `json:"channels"`
	Tags      map[string]string `json:"tags"`
}

type probeFormat struct {
	FormatName string          `json:"format_name"`
	Duration   json.RawMessage `json:"duration"`
	Size       json.RawMessage `json:"size"`
}

func (r *Runner) inspect(ctx context.Context, request mediaRequest) (app.MediaInfo, error) {
	args := []string{"-v", "error", "-print_format", "json", "-show_format", "-show_streams"}
	args = append(args, request.HeaderArg...)
	args = append(args, request.CookieArg...)
	args = append(args, request.URL)

	stdout, stderr, err := r.runJSONCommand(ctx, r.toolPath("ffprobe"), args)
	if err != nil {
		return app.MediaInfo{}, commandError("ffprobe", err, stderr, request)
	}
	var document probeDocument
	if err := json.Unmarshal(stdout, &document); err != nil {
		return app.MediaInfo{}, errors.New("ffprobe returned invalid metadata")
	}

	info := app.MediaInfo{SourceDisplay: request.Display, Format: document.Format.FormatName}
	info.DurationMS = parseDuration(document.Format.Duration)
	info.SizeBytes = parseInteger(document.Format.Size)
	if len(document.Streams) > 0 {
		info.Streams = make([]app.MediaStream, 0, len(document.Streams))
	}
	for _, stream := range document.Streams {
		language := ""
		for key, value := range stream.Tags {
			if strings.EqualFold(key, "language") {
				language = value
				break
			}
		}
		info.Streams = append(info.Streams, app.MediaStream{
			Index:    stream.Index,
			Kind:     stream.CodecType,
			Codec:    stream.CodecName,
			Language: language,
			Width:    stream.Width,
			Height:   stream.Height,
			Channels: stream.Channels,
		})
	}
	return info, nil
}

func ffmpegArgs(request mediaRequest, spec outputSpec, tempPath, format string, hlsInput bool, caps ffmpegcap.Capabilities) ([]string, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:1"}
	if parsed, err := url.Parse(request.URL); err == nil && parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		// Add only options the concrete FFmpeg build reports as supported.
		// This keeps older or distro-patched builds usable instead of failing
		// downloads with an "Option not found" error.
		if caps.Reconnect {
			args = append(args, "-reconnect", "1")
		}
		if caps.ReconnectStreamed {
			args = append(args, "-reconnect_streamed", "1")
		}
		if caps.ReconnectOnNetworkError {
			args = append(args, "-reconnect_on_network_error", "1")
		}
		if caps.ReconnectOnHTTPError {
			args = append(args, "-reconnect_on_http_error", "429,500,502,503,504")
		}
		if caps.ReconnectDelayMax {
			args = append(args, "-reconnect_delay_max", strconv.Itoa(ffmpegReconnectDelayMax))
		}
		if caps.ReconnectMaxRetries {
			args = append(args, "-reconnect_max_retries", strconv.Itoa(ffmpegReconnectRetries))
		}
		if caps.ReconnectDelayTotalMax {
			args = append(args, "-reconnect_delay_total_max", strconv.Itoa(ffmpegReconnectTotalMax))
		}
		if caps.RespectRetryAfter {
			args = append(args, "-respect_retry_after", "1")
		}
		if hlsInput && caps.HLSSegmentMaxRetry {
			args = append(args, "-seg_max_retry", strconv.Itoa(ffmpegReconnectRetries))
		}
	}
	args = append(args, request.HeaderArg...)
	args = append(args, request.CookieArg...)
	args = append(args,
		"-i", request.URL,
		// Let FFmpeg's default stream selection choose the highest-resolution
		// video and the audio stream with the most channels. Explicitly
		// disabling subtitles/data keeps the copy-only MP4/MKV path stable for
		// HLS masters with multiple renditions and optional attachments.
		"-sn", "-dn",
		"-c", "copy",
		"-f", muxerName(format),
		"-y", tempPath,
	)
	return args, nil
}

func muxerName(format string) string {
	if format == "mkv" {
		return "matroska"
	}
	return "mp4"
}

func parseDuration(raw json.RawMessage) *int64 {
	seconds, ok := parseFloat(raw)
	if !ok || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return nil
	}
	value := int64(math.Round(seconds * 1000))
	return &value
}

func parseInteger(raw json.RawMessage) *int64 {
	value, ok := parseFloat(raw)
	if !ok || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value > math.MaxInt64 {
		return nil
	}
	parsed := int64(math.Round(value))
	return &parsed
}

func parseFloat(raw json.RawMessage) (float64, bool) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, false
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return 0, false
		}
		value = strings.TrimSpace(decoded)
	}
	if value == "" || strings.EqualFold(value, "N/A") {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

func (r *Runner) runJSONCommand(ctx context.Context, tool string, args []string) ([]byte, string, error) {
	if err := contextErr(ctx); err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	var stdout bytes.Buffer
	var stderr boundedBuffer
	cmd.Stdout = &stdout
	stderr.limit = maxStderrBytes
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, stderr.String(), toolStartError(tool, err)
	}
	err := cmd.Wait()
	if ctxErr := contextErr(ctx); ctxErr != nil {
		return nil, stderr.String(), ctxErr
	}
	return stdout.Bytes(), stderr.String(), err
}

func (r *Runner) runFFmpeg(ctx context.Context, args []string, state *progressState, report func(Progress), request mediaRequest) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, r.toolPath("ffmpeg"), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg output: %w", err)
	}
	var stderr boundedBuffer
	stderr.limit = maxStderrBytes
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return toolStartError(r.toolPath("ffmpeg"), err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 256*1024)
	for scanner.Scan() {
		if state.update(scanner.Text()) && report != nil {
			report(state.snapshot())
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctxErr := contextErr(ctx); ctxErr != nil {
		return ctxErr
	}
	if scanErr != nil {
		return fmt.Errorf("read ffmpeg progress: %w", scanErr)
	}
	if waitErr != nil {
		return commandError("ffmpeg", waitErr, stderr.String(), request)
	}
	return nil
}

func (r *Runner) toolPath(tool string) string {
	if tool == "ffprobe" {
		return defaultToolPath(r.FFprobePath, "ffprobe")
	}
	return defaultToolPath(r.FFmpegPath, "ffmpeg")
}

type progressState struct {
	durationMS int64
	bytesDone  int64
	outTimeMS  int64
	fraction   float64
}

func (s *progressState) snapshot() Progress {
	frac := UnknownFraction
	if s.durationMS > 0 {
		frac = s.fraction
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
	}
	return Progress{
		Fraction:   frac,
		BytesDone:  s.bytesDone,
		DurationMS: s.durationMS,
		OutTimeMS:  s.outTimeMS,
	}
}

func (s *progressState) update(line string) bool {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	switch key {
	case "total_size":
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed >= 0 {
			s.bytesDone = parsed
			return true
		}
	case "out_time_ms", "out_time_us":
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed >= 0 {
			// FFmpeg labels this field *_ms, but reports microseconds for
			// compatibility. Convert to the public millisecond unit.
			s.outTimeMS = parsed / 1000
			s.updateFraction()
			return true
		}
	case "out_time":
		if parsed, ok := parseClock(value); ok {
			s.outTimeMS = parsed
			s.updateFraction()
			return true
		}
	case "progress":
		if value == "end" {
			s.updateFraction()
		}
		return true
	}
	return false
}

func (s *progressState) updateFraction() {
	if s.durationMS > 0 {
		s.fraction = float64(s.outTimeMS) / float64(s.durationMS)
		if s.fraction > 1 {
			s.fraction = 1
		}
	}
}

func parseClock(value string) (int64, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, errH := strconv.ParseInt(parts[0], 10, 64)
	minutes, errM := strconv.ParseInt(parts[1], 10, 64)
	seconds, errS := strconv.ParseFloat(parts[2], 64)
	if errH != nil || errM != nil || errS != nil || hours < 0 || minutes < 0 || seconds < 0 {
		return 0, false
	}
	return int64(math.Round((float64(hours)*3600 + float64(minutes)*60 + seconds) * 1000)), true
}

type outputSpec struct {
	dir    string
	path   string
	format string
}

func normalizeFormat(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "mp4"
	}
	if value != "mp4" && value != "mkv" {
		return "", ErrUnsupportedFormat
	}
	return value, nil
}

func prepareOutput(rawDir, rawName, sourceURL, format string) (outputSpec, error) {
	dir := strings.TrimSpace(rawDir)
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return outputSpec{}, fmt.Errorf("resolve output directory: %w", err)
	}

	name := strings.TrimSpace(rawName)
	if name == "" {
		name = defaultOutputName(sourceURL, format)
	}
	if err := validateOutputName(name); err != nil {
		return outputSpec{}, err
	}
	if filepath.Ext(name) == "" {
		name += "." + format
	}
	return outputSpec{dir: absDir, path: filepath.Join(absDir, name), format: format}, nil
}

func validateOutputName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, 0) {
		return errors.New("output name is required")
	}
	if strings.ContainsAny(name, `/\\`) || filepath.Base(name) != name || strings.HasPrefix(name, "-") {
		return errors.New("output name must be a safe file name")
	}
	return nil
}

func defaultOutputName(sourceURL, format string) string {
	parsed, err := url.Parse(sourceURL)
	if err == nil {
		name := filepath.Base(parsed.Path)
		if name != "." && name != "/" && name != "" {
			if decoded, decodeErr := url.PathUnescape(name); decodeErr == nil {
				name = decoded
			}
			name = strings.TrimSpace(name)
			if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\\`) {
				if ext := filepath.Ext(name); ext != "" {
					name = strings.TrimSuffix(name, ext)
				}
				return name + "." + format
			}
		}
	}
	return "download." + format
}

func tempPattern(taskID, format string) string {
	clean := sanitizeTaskID(taskID)
	return ".riflo-" + clean + "-*" + "." + format
}

func sanitizeTaskID(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	if builder.Len() == 0 {
		return "task"
	}
	return builder.String()
}

func ensureDestinationAbsent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return ErrOutputExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check output destination: %w", err)
	}
	return nil
}

func publishNoReplace(tempPath, outputPath string) error {
	// A same-directory hard-link creation is atomic and fails with EEXIST,
	// unlike os.Rename which would replace a destination on Unix. Removing the
	// temporary name after linking leaves the completed file published without
	// a partial-output window or overwrite race.
	if err := os.Link(tempPath, outputPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrOutputExists
		}
		return fmt.Errorf("publish output: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove temporary output: %w", err)
	}
	return nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func toolStartError(tool string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrToolUnavailable, tool)
	}
	return fmt.Errorf("start %s: %w", tool, err)
}

func commandError(tool string, waitErr error, stderr string, request mediaRequest) error {
	if waitErr == nil {
		return nil
	}
	if isAccessDeniedStderr(stderr) {
		return fmt.Errorf("%w: %s was denied access", ErrAccessDenied, tool)
	}
	if isRateLimitedStderr(stderr) {
		return fmt.Errorf("%w: %s was rate limited", ErrRateLimited, tool)
	}
	if isTimeoutStderr(stderr) {
		return fmt.Errorf("%w: %s timed out", ErrTimeout, tool)
	}
	if isNetworkStderr(stderr) {
		return fmt.Errorf("%w: %s network request failed", ErrNetwork, tool)
	}
	if isHTTPStderr(stderr) {
		if isExpiredURLStderr(stderr) {
			return fmt.Errorf("%w: %s media URL is unavailable", ErrURLExpired, tool)
		}
		return fmt.Errorf("%w: %s returned an HTTP error", ErrHTTPStatus, tool)
	}
	if isInvalidPlaylistStderr(stderr) {
		return fmt.Errorf("%w: %s returned an invalid playlist", ErrInvalidPlaylist, tool)
	}
	summary := sanitizeStderr(stderr, request)
	if summary == "" {
		return fmt.Errorf("%s failed", tool)
	}
	return fmt.Errorf("%s failed: %s", tool, summary)
}

func isAccessDeniedStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{
		"http error 401", "http error 403", "http/1.1 401", "http/1.1 403",
		"401 unauthorized", "403 forbidden", "server returned 401", "server returned 403",
		"status code 401", "status code 403",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isRateLimitedStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "http error 429") || strings.Contains(lower, "429 too many") || strings.Contains(lower, "status code 429")
}

func isTimeoutStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{"timed out", "timeout", "operation timed out"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isNetworkStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{"network is unreachable", "connection refused", "connection reset", "could not resolve host", "name or service not known", "tls handshake", "temporary failure in name resolution"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isHTTPStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "http error") || strings.Contains(lower, "http/1.1") || strings.Contains(lower, "server returned") || strings.Contains(lower, "status code")
}

func isExpiredURLStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{
		"http error 404", "http error 410", "http/1.1 404", "http/1.1 410",
		"404 not found", "410 gone", "server returned 404", "server returned 410",
		"status code 404", "status code 410",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isInvalidPlaylistStderr(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{"invalid data found", "invalid playlist", "failed to parse playlist", "not a valid m3u8"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.Buffer.Write(p[:remaining])
		} else {
			_, _ = b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

var stderrURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func sanitizeStderr(stderr string, request mediaRequest) string {
	for _, secret := range append([]string{request.URL}, request.Secrets...) {
		if secret != "" {
			stderr = strings.ReplaceAll(stderr, secret, "[redacted url]")
		}
	}
	// The values are not retained in mediaRequest, so redact any URL-shaped
	// token and any line that looks like a sensitive request header as well.
	stderr = stderrURLPattern.ReplaceAllStringFunc(stderr, func(value string) string {
		parsed, err := url.Parse(value)
		if err != nil {
			return "[redacted url]"
		}
		return displayURL(parsed)
	})
	lines := strings.Split(stderr, "\n")
	for index, line := range lines {
		if colon := strings.IndexByte(line, ':'); colon > 0 {
			name := strings.TrimSpace(line[:colon])
			switch strings.ToLower(name) {
			case "cookie", "authorization", "proxy-authorization", "referer", "origin", "user-agent":
				lines[index] = line[:colon+1] + " [redacted]"
			}
		}
	}
	stderr = strings.Join(lines, " ")
	stderr = strings.Join(strings.Fields(stderr), " ")
	if len(stderr) > maxErrorBytes {
		stderr = stderr[:maxErrorBytes] + "..."
	}
	return strings.TrimSpace(stderr)
}


func (r *Runner) capabilities(ctx context.Context) ffmpegcap.Capabilities {
	if r == nil {
		return ffmpegcap.Capabilities{}
	}
	r.capOnce.Do(func() {
		r.caps, r.capErr = ffmpegcap.Detect(ctx, r.toolPath("ffmpeg"))
	})
	if r.capErr != nil {
		return ffmpegcap.Capabilities{}
	}
	return r.caps
}
