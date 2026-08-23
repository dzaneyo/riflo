package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dzaneyo/riflo/internal/app"
)

func TestParseHLSMasterSummaryRedactsVariantQueries(t *testing.T) {
	base, err := url.Parse("https://media.example.test/path/master.m3u8?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,AVERAGE-BANDWIDTH=700000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\"\n" +
		"low/index.m3u8?token=variant-secret\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=4200000,RESOLUTION=1920x1080\n" +
		"https://cdn.example.test/high/index.m3u8?sig=another-secret\n")
	info, err := parseHLSPlaylist(body, base)
	if err != nil {
		t.Fatalf("parseHLSPlaylist() error = %v", err)
	}
	if info.PlaylistType != "master" || info.Availability != "unknown" {
		t.Fatalf("summary type/status = %+v", info)
	}
	if len(info.Variants) != 2 {
		t.Fatalf("variant count = %d", len(info.Variants))
	}
	low, high := info.Variants[0], info.Variants[1]
	if low.Index != 0 || low.Bandwidth != 800000 || low.AverageBandwidth != 700000 || low.Width != 640 || low.Height != 360 {
		t.Fatalf("low variant = %+v", low)
	}
	if high.Index != 1 || high.Bandwidth != 4200000 || high.Width != 1920 || high.Height != 1080 {
		t.Fatalf("high variant = %+v", high)
	}
	for _, value := range []string{low.URLDisplay, high.URLDisplay, fmt.Sprintf("%+v", info)} {
		if strings.Contains(value, "secret") || strings.Contains(value, "sig=") || strings.Contains(value, "token=") {
			t.Fatalf("summary leaked credential-bearing URL data: %q", value)
		}
	}
	if low.URLDisplay != "https://media.example.test/path/low/index.m3u8" || high.URLDisplay != "https://cdn.example.test/high/index.m3u8" {
		t.Fatalf("variant displays = %q, %q", low.URLDisplay, high.URLDisplay)
	}
}

func TestParseHLSMediaSummary(t *testing.T) {
	base, _ := url.Parse("https://media.example.test/video/playlist.m3u8")
	body := []byte("#EXTM3U\n" +
		"#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"keys/secret.key?token=private\"\n" +
		"#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"720@0\"\n" +
		"#EXTINF:4.000,\n" +
		"segment-1.m4s?token=private\n" +
		"#EXT-X-BYTERANGE:1200@720\n" +
		"#EXTINF:4.000,\n" +
		"segment-2.m4s?token=private\n" +
		"#EXT-X-ENDLIST\n")
	info, err := parseHLSPlaylist(body, base)
	if err != nil {
		t.Fatalf("parseHLSPlaylist() error = %v", err)
	}
	if info.PlaylistType != "media" || info.Availability != "vod" || info.SegmentFormat != "fmp4" || !info.ByteRange || info.SegmentCount != 2 {
		t.Fatalf("summary = %+v", info)
	}
	if len(info.EncryptionMethods) != 1 || info.EncryptionMethods[0] != "AES-128" {
		t.Fatalf("encryption = %#v", info.EncryptionMethods)
	}
}

func TestParseHLSLiveTSAndMixedFormat(t *testing.T) {
	base, _ := url.Parse("https://media.example.test/live.m3u8")
	body := []byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nfirst.ts\n#EXTINF:6,\nsecond.mp4\n")
	info, err := parseHLSPlaylist(body, base)
	if err != nil {
		t.Fatalf("parseHLSPlaylist() error = %v", err)
	}
	if info.PlaylistType != "media" || info.Availability != "live" || info.SegmentCount != 2 || info.SegmentFormat != "mixed" {
		t.Fatalf("summary = %+v", info)
	}
}

func TestInspectHLSPreflightUsesTransientHeaders(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Referer") != "https://watch.example.test/page?id=private" {
			t.Errorf("Referer = %q", r.Header.Get("Referer"))
		}
		if r.Header.Get("User-Agent") != "riflo-test-agent" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Cookie") != "session=private" {
			t.Errorf("Cookie = %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100,RESOLUTION=320x180\nvariant.m3u8?sig=secret\n")
	}))
	defer server.Close()
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"format_name\":\"hls\"},\"streams\":[]}'")
	runner := NewRunner(probe, "missing-ffmpeg")
	info, err := runner.Inspect(context.Background(), app.InspectRequest{
		URL:       server.URL + "/master.m3u8?token=secret",
		Referer:   "https://watch.example.test/page?id=private",
		UserAgent: "riflo-test-agent",
		Cookie:    "session=private",
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if requests.Load() != 1 || info.HLS == nil || len(info.HLS.Variants) != 1 {
		t.Fatalf("requests=%d info=%+v", requests.Load(), info)
	}
	if strings.Contains(info.HLS.Variants[0].URLDisplay, "secret") {
		t.Fatalf("variant display leaked secret: %q", info.HLS.Variants[0].URLDisplay)
	}
}

func TestInspectExtensionlessHLSUsesFFprobeDiscriminator(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/playlist" {
			t.Errorf("playlist path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n")
	}))
	defer server.Close()
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"format_name\":\"hls\"},\"streams\":[]}'")
	runner := NewRunner(probe, "missing-ffmpeg")
	info, err := runner.Inspect(context.Background(), app.InspectRequest{URL: server.URL + "/playlist?token=secret"})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if requests.Load() != 1 || info.HLS == nil || info.HLS.PlaylistType != "media" {
		t.Fatalf("requests=%d info=%+v", requests.Load(), info)
	}
}

func TestInspectHLSRedirectDropsCredentialsAcrossOrigins(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			leaked.Store(true)
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n")
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/playlist.m3u8?token=redirect-secret", http.StatusFound)
	}))
	defer source.Close()
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"format_name\":\"hls\"},\"streams\":[]}'")
	runner := NewRunner(probe, "missing-ffmpeg")
	info, err := runner.Inspect(context.Background(), app.InspectRequest{
		URL:     source.URL + "/master.m3u8?token=source-secret",
		Referer: "https://watch.example.test/page?id=private",
		Cookie:  "session=private",
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if leaked.Load() || info.HLS == nil || info.HLS.Availability != "vod" {
		t.Fatalf("cross-origin redirect leaked credentials or lost summary: leaked=%v info=%+v", leaked.Load(), info)
	}
}

func TestInspectHLSAccessDeniedIsStable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden session=private token=secret", http.StatusForbidden)
	}))
	defer server.Close()
	probeMarker := filepath.Join(t.TempDir(), "ffprobe-ran")
	probe := fakeExecutable(t, fmt.Sprintf("printf x > %s", shellQuote(probeMarker)))
	runner := NewRunner(probe, "missing-ffmpeg")
	_, err := runner.Inspect(context.Background(), app.InspectRequest{
		URL:    server.URL + "/master.m3u8?token=secret",
		Cookie: "session=private",
	})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Inspect() error = %v, want ErrAccessDenied", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "session") {
		t.Fatalf("access error leaked credentials: %v", err)
	}
	if _, statErr := os.Stat(probeMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ffprobe ran after preflight access denial, stat error = %v", statErr)
	}
}

func TestCommandErrorMapsHTTPAccessDenied(t *testing.T) {
	probe := fakeExecutable(t, "printf '%s' 'HTTP error 403 Forbidden' >&2\nexit 1")
	runner := NewRunner(probe, "missing-ffmpeg")
	_, err := runner.Inspect(context.Background(), app.InspectRequest{URL: "https://media.example.test/video"})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Inspect() error = %v, want ErrAccessDenied", err)
	}
}

func TestFFmpegArgsUseDefaultBestStreamSelection(t *testing.T) {
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"duration\":\"1.0\"}}'")
	argsPath := filepath.Join(t.TempDir(), "args")
	ffmpeg := fakeExecutable(t, fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\nfor arg do last=\"$arg\"; done\nprintf 'progress=end\\n'\nprintf media > \"$last\"", shellQuote(argsPath)))
	dir := t.TempDir()
	runner := NewRunner(probe, ffmpeg)
	if _, err := runner.Download(context.Background(), "task", app.CreateTaskRequest{
		URL: "https://media.example.test/master.m3u8", OutputDir: dir, OutputName: "saved.mp4", Format: "mp4",
	}, nil); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	for _, line := range lines {
		if line == "-map" || line == "0" {
			t.Fatalf("ffmpeg args retained broad stream map: %q", string(args))
		}
	}
	if !containsString(lines, "-sn") || !containsString(lines, "-dn") {
		t.Fatalf("ffmpeg args did not disable optional subtitles/data: %q", string(args))
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
