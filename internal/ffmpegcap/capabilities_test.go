package ffmpegcap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectCapabilitiesFromHelp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
case "$*" in
  *protocol=http*)
    cat <<'EOF'
-reconnect
-reconnect_streamed
-reconnect_on_network_error
-reconnect_on_http_error
-reconnect_delay_max
-reconnect_max_retries
-reconnect_delay_total_max
-respect_retry_after
-cookies
EOF
    ;;
  *demuxer=hls*)
    echo "-seg_max_retry"
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	caps, err := Detect(context.Background(), path)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if !caps.ReliableHTTP() || !caps.ReconnectMaxRetries || !caps.ReconnectDelayTotalMax ||
		!caps.RespectRetryAfter || !caps.HLSSegmentMaxRetry || !caps.Cookies {
		t.Fatalf("capabilities = %+v", caps)
	}
}
