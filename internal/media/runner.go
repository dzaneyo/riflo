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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
)

const (
	// UnknownFraction is used when the input does not expose a usable duration.
	// BytesDone, DurationMS, and OutTimeMS use zero for an unknown value.
	UnknownFraction = -1.0

	maxHeaderValue  = 64 * 1024
	maxStderrBytes  = 8 * 1024
	maxErrorBytes   = 4 * 1024
	maxHLSBodyBytes = 2 * 1024 * 1024
	hlsProbeTimeout = 8 * time.Second
	maxHLSRedirects = 5
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
	// ErrPlaylistTooLarge prevents a preflight from reading an unbounded body.
	ErrPlaylistTooLarge = errors.New("HLS playlist is too large")
)

// Config controls the executable paths used by Runner. Empty paths resolve
// ffprobe and ffmpeg through PATH at execution time.
type Config struct {
	FFprobePath string
	FFmpegPath  string
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
	request, err := normalizeRequest(req.URL, req.Referer, req.UserAgent, req.Cookie)
	if err != nil {
		return app.MediaInfo{}, err
	}
	var hls *app.HLSInfo
	preflightAttempted := false
	if isLikelyHLSURL(request.URL) {
		preflightAttempted = true
		preflight, preflightErr := r.inspectHLS(ctx, request)
		if preflightErr != nil {
			if errors.Is(preflightErr, ErrAccessDenied) || errors.Is(preflightErr, context.Canceled) || errors.Is(preflightErr, context.DeadlineExceeded) && ctx.Err() != nil {
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
			if errors.Is(preflightErr, ErrAccessDenied) || errors.Is(preflightErr, context.Canceled) || errors.Is(preflightErr, context.DeadlineExceeded) && ctx.Err() != nil {
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
	request, err := normalizeRequest(req.URL, req.Referer, req.UserAgent, req.Cookie)
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

	args, err := ffmpegArgs(request, spec, tempPath, format)
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

type mediaRequest struct {
	URL       string
	Display   string
	Referer   string
	UserAgent string
	Cookie    string
	HeaderArg []string
	Secrets   []string
}

func normalizeRequest(rawURL, referer, userAgent, cookie string) (mediaRequest, error) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := parseMediaURL(rawURL)
	if err != nil {
		return mediaRequest{}, err
	}
	for _, header := range []struct {
		name  string
		value string
	}{
		{name: "Referer", value: referer},
		{name: "User-Agent", value: userAgent},
		{name: "Cookie", value: cookie},
	} {
		if err := validateHeaderValue(header.name, header.value); err != nil {
			return mediaRequest{}, err
		}
	}
	headerArg, err := makeHeaderArg(referer, userAgent, cookie)
	if err != nil {
		return mediaRequest{}, err
	}
	secrets := make([]string, 0, 3)
	for _, value := range []string{referer, userAgent, cookie} {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	return mediaRequest{
		URL: rawURL, Display: displayURL(parsed), Referer: referer, UserAgent: userAgent,
		Cookie: cookie, HeaderArg: headerArg, Secrets: secrets,
	}, nil
}

func parseMediaURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: url is required", ErrInvalidURL)
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("%w: url contains invalid control characters", ErrInvalidURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrInvalidURL
	}
	return parsed, nil
}

func displayURL(parsed *url.URL) string {
	clone := *parsed
	clone.User = nil
	clone.RawQuery = ""
	clone.ForceQuery = false
	clone.Fragment = ""
	return clone.String()
}

func validateHeaderValue(name, value string) error {
	if len(value) > maxHeaderValue {
		return fmt.Errorf("%s header is too long", name)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s header contains invalid control characters", name)
	}
	return nil
}

func makeHeaderArg(referer, userAgent, cookie string) ([]string, error) {
	for _, header := range []struct {
		name  string
		value string
	}{
		{name: "Referer", value: referer},
		{name: "User-Agent", value: userAgent},
		{name: "Cookie", value: cookie},
	} {
		if err := validateHeaderValue(header.name, header.value); err != nil {
			return nil, err
		}
	}
	headers := make([]string, 0, 3)
	if referer != "" {
		headers = append(headers, "Referer: "+referer)
	}
	if userAgent != "" {
		headers = append(headers, "User-Agent: "+userAgent)
	}
	if cookie != "" {
		headers = append(headers, "Cookie: "+cookie)
	}
	if len(headers) == 0 {
		return nil, nil
	}
	// FFmpeg's HTTP protocol expects a CRLF-delimited header block. Values have
	// already been checked for CR/LF, so a caller cannot inject another header.
	return []string{"-headers", strings.Join(headers, "\r\n") + "\r\n"}, nil
}

func isLikelyHLSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	return strings.HasSuffix(path, ".m3u8") || strings.Contains(path, ".m3u8/")
}

// inspectHLS performs one bounded playlist request for an explicit m3u8 URL.
// It is intentionally separate from ffprobe: a failed diagnostic request is
// allowed to fall back to ffprobe, while upstream 401/403 responses remain
// actionable and stable for the API layer.
func (r *Runner) inspectHLS(ctx context.Context, request mediaRequest) (*app.HLSInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, hlsProbeTimeout)
	defer cancel()
	hlsRequest, err := http.NewRequestWithContext(probeCtx, http.MethodGet, request.URL, nil)
	if err != nil {
		return nil, err
	}
	hlsRequest.Header.Set("Accept", "application/vnd.apple.mpegurl, application/x-mpegURL, text/plain;q=0.8")
	setTransientHeader(hlsRequest.Header, "Referer", request.Referer)
	setTransientHeader(hlsRequest.Header, "User-Agent", request.UserAgent)
	setTransientHeader(hlsRequest.Header, "Cookie", request.Cookie)

	initialURL, _ := url.Parse(request.URL)
	client := &http.Client{
		Timeout: hlsProbeTimeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= maxHLSRedirects {
				return errors.New("too many HLS redirects")
			}
			if initialURL != nil && sameHTTPOrigin(initialURL, next.URL) {
				setTransientHeader(next.Header, "Referer", request.Referer)
				setTransientHeader(next.Header, "User-Agent", request.UserAgent)
				setTransientHeader(next.Header, "Cookie", request.Cookie)
				return nil
			}
			// A playlist CDN redirect may cross origins. Do not forward user
			// supplied Cookie/Referer values to a new origin; the latter may
			// itself contain a signed query. User-Agent is not credential data.
			next.Header.Del("Referer")
			next.Header.Del("Cookie")
			setTransientHeader(next.Header, "User-Agent", request.UserAgent)
			return nil
		},
	}
	response, err := client.Do(hlsRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: upstream returned %d", ErrAccessDenied, response.StatusCode)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%w: upstream returned %d", ErrHTTPStatus, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHLSBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHLSBodyBytes {
		return nil, ErrPlaylistTooLarge
	}
	finalURL := response.Request.URL
	if finalURL == nil {
		finalURL = initialURL
	}
	info, err := parseHLSPlaylist(body, finalURL)
	if errors.Is(err, errNotHLSPlaylist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &info, nil
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

func ffmpegArgs(request mediaRequest, spec outputSpec, tempPath, format string) ([]string, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:1"}
	args = append(args, request.HeaderArg...)
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
			case "cookie", "authorization", "proxy-authorization", "referer", "user-agent":
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
