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

	return Capabilities{
		Reconnect:               hasOption(httpHelp, "reconnect"),
		ReconnectStreamed:       hasOption(httpHelp, "reconnect_streamed"),
		ReconnectOnNetworkError: hasOption(httpHelp, "reconnect_on_network_error"),
		ReconnectOnHTTPError:    hasOption(httpHelp, "reconnect_on_http_error"),
		ReconnectDelayMax:       hasOption(httpHelp, "reconnect_delay_max"),
		ReconnectMaxRetries:     hasOption(httpHelp, "reconnect_max_retries"),
		ReconnectDelayTotalMax:  hasOption(httpHelp, "reconnect_delay_total_max"),
		RespectRetryAfter:       hasOption(httpHelp, "respect_retry_after"),
		HLSSegmentMaxRetry:      hasOption(hlsHelp, "seg_max_retry"),
		Cookies:                 hasOption(httpHelp, "cookies"),
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


func hasOption(helpText, option string) bool {
	for _, line := range strings.Split(helpText, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.TrimLeft(fields[0], "-") == option {
			return true
		}
	}
	return false
}
