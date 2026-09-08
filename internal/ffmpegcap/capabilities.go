// Package ffmpegcap detects optional FFmpeg protocol and demuxer capabilities.
package ffmpegcap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Capabilities describes the optional HTTP/HLS controls riflo can safely use.
// Zero values are intentionally conservative so older FFmpeg builds continue
// to work without receiving unknown command-line options.
type Capabilities struct {
	Reconnect               bool
	ReconnectStreamed       bool
	ReconnectOnNetworkError bool
	ReconnectOnHTTPError    bool
	ReconnectDelayMax       bool
	ReconnectMaxRetries     bool
	ReconnectDelayTotalMax  bool
	RespectRetryAfter       bool
	HLSSegmentMaxRetry      bool
	Cookies                 bool
}

// ReliableHTTP reports whether the bounded retry controls used by riflo are
// all available. Individual flags remain useful because riflo degrades
// gracefully when only some options exist.
func (c Capabilities) ReliableHTTP() bool {
	return c.Reconnect &&
		c.ReconnectStreamed &&
		c.ReconnectOnNetworkError &&
		c.ReconnectOnHTTPError &&
		c.ReconnectDelayMax
}

// Detect asks the concrete FFmpeg binary which protocol/demuxer options it
// supports instead of relying on a version number. This also works for distro
// builds that backport individual options.
func Detect(ctx context.Context, ffmpegPath string) (Capabilities, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(ffmpegPath) == "" {
		ffmpegPath = "ffmpeg"
	}

	httpHelp, err := help(ctx, ffmpegPath, "protocol=http")
	if err != nil {
		return Capabilities{}, fmt.Errorf("inspect FFmpeg HTTP capabilities: %w", err)
	}
	hlsHelp, err := help(ctx, ffmpegPath, "demuxer=hls")
	if err != nil {
		return Capabilities{}, fmt.Errorf("inspect FFmpeg HLS capabilities: %w", err)
	}

	has := func(text, option string) bool {
		return strings.Contains(text, option)
	}
	return Capabilities{
		Reconnect:               has(httpHelp, "reconnect"),
		ReconnectStreamed:       has(httpHelp, "reconnect_streamed"),
		ReconnectOnNetworkError: has(httpHelp, "reconnect_on_network_error"),
		ReconnectOnHTTPError:    has(httpHelp, "reconnect_on_http_error"),
		ReconnectDelayMax:       has(httpHelp, "reconnect_delay_max"),
		ReconnectMaxRetries:     has(httpHelp, "reconnect_max_retries"),
		ReconnectDelayTotalMax:  has(httpHelp, "reconnect_delay_total_max"),
		RespectRetryAfter:       has(httpHelp, "respect_retry_after"),
		HLSSegmentMaxRetry:      has(hlsHelp, "seg_max_retry"),
		Cookies:                 has(httpHelp, "cookies"),
	}, nil
}

func help(ctx context.Context, path, topic string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-h", topic)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%s -h %s: %w", path, topic, err)
	}
	return string(output), nil
}
