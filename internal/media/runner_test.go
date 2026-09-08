package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/ffmpegcap"
)

func TestInspectMapsProbeJSONAndRedactsSource(t *testing.T) {
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"format_name\":\"hls\",\"duration\":\"12.345\",\"size\":\"9876\"},\"streams\":[{\"index\":0,\"codec_type\":\"video\",\"codec_name\":\"h264\",\"width\":1920,\"height\":1080,\"tags\":{\"language\":\"eng\"}},{\"index\":1,\"codec_type\":\"audio\",\"codec_name\":\"aac\",\"channels\":2,\"tags\":{\"LANGUAGE\":\"und\"}}]}'")
	runner := NewRunner(probe, "missing-ffmpeg")
	info, err := runner.Inspect(context.Background(), app.InspectRequest{
		URL:       "https://media.example.test/video/master.m3u8?token=secret#fragment",
		Referer:   "https://media.example.test/watch?id=private",
		UserAgent: "riflo-test-agent",
		Cookie:    "session=private",
	})
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if info.SourceDisplay != "https://media.example.test/video/master.m3u8" {
		t.Fatalf("SourceDisplay = %q", info.SourceDisplay)
	}
	if info.Format != "hls" {
		t.Fatalf("Format = %q", info.Format)
	}
	if info.DurationMS == nil || *info.DurationMS != 12345 {
		t.Fatalf("DurationMS = %v", info.DurationMS)
	}
	if info.SizeBytes == nil || *info.SizeBytes != 9876 {
		t.Fatalf("SizeBytes = %v", info.SizeBytes)
	}
	if len(info.Streams) != 2 {
		t.Fatalf("stream count = %d", len(info.Streams))
	}
	if got := info.Streams[0]; got.Kind != "video" || got.Codec != "h264" || got.Language != "eng" || got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("video stream = %+v", got)
	}
}

func TestDownloadSuccessProgressAndOutput(t *testing.T) {
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"duration\":\"4.000\"},\"streams\":[]}'")
	ffmpeg := fakeExecutable(t, "for arg do last=\"$arg\"; done\nprintf 'total_size=12\\nout_time_ms=2500000\\nprogress=continue\\nprogress=end\\n'\nprintf 'fake media' > \"$last\"")
	dir := t.TempDir()
	runner := NewRunner(probe, ffmpeg)
	var snapshots []Progress
	result, err := runner.Download(context.Background(), "task/unsafe", app.CreateTaskRequest{
		URL:        "https://media.example.test/video/master.m3u8?sig=private",
		OutputDir:  dir,
		OutputName: "saved",
		Format:     "mp4",
	}, func(progress Progress) { snapshots = append(snapshots, progress) })
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	wantPath := filepath.Join(dir, "saved.mp4")
	if result.OutputPath != wantPath || result.DurationMS != 2500 {
		t.Fatalf("result = %+v, want path %q and duration 2500", result, wantPath)
	}
	content, err := os.ReadFile(wantPath)
	if err != nil || string(content) != "fake media" {
		t.Fatalf("output = %q, read error = %v", content, err)
	}
	if len(snapshots) == 0 {
		t.Fatal("Download emitted no progress")
	}
	last := snapshots[len(snapshots)-1]
	if last.Fraction != 1 || last.BytesDone != 12 || last.DurationMS != 4000 || last.OutTimeMS != 2500 {
		t.Fatalf("last progress = %+v", last)
	}
	for _, entry := range mustReadDir(t, dir) {
		if strings.HasPrefix(entry.Name(), ".riflo-") {
			t.Fatalf("temporary file was left behind: %s", entry.Name())
		}
	}
}

func TestDownloadDoesNotOverwriteExistingOutput(t *testing.T) {
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"duration\":\"1.0\"}}'")
	marker := filepath.Join(t.TempDir(), "ffmpeg-ran")
	ffmpeg := fakeExecutable(t, fmt.Sprintf("printf x > %s\nexit 1", shellQuote(marker)))
	dir := t.TempDir()
	target := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(probe, ffmpeg)
	_, err := runner.Download(context.Background(), "task", app.CreateTaskRequest{
		URL:        "https://example.test/video",
		OutputDir:  dir,
		OutputName: "video.mp4",
		Format:     "mp4",
	}, nil)
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("Download() error = %v, want ErrOutputExists", err)
	}
	content, readErr := os.ReadFile(target)
	if readErr != nil || string(content) != "original" {
		t.Fatalf("existing output changed: %q (error %v)", content, readErr)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ffmpeg ran despite existing output, stat error = %v", statErr)
	}
}

func TestDownloadFailureDoesNotLeakCookieAndCleansTemp(t *testing.T) {
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"duration\":\"1.0\"}}'")
	requestURL := "https://example.test/video.m3u8?token=secret-token"
	cookie := "session=super-secret"
	ffmpeg := fakeExecutable(t, fmt.Sprintf("printf 'failed %s\\nCookie: %s\\nUser-Agent: private-agent\\n' %s %s >&2\nexit 7", shellQuote(requestURL), shellQuote(cookie), shellQuote(requestURL), shellQuote(cookie)))
	dir := t.TempDir()
	runner := NewRunner(probe, ffmpeg)
	_, err := runner.Download(context.Background(), "task", app.CreateTaskRequest{
		URL:        requestURL,
		Cookie:     cookie,
		UserAgent:  "private-agent",
		OutputDir:  dir,
		OutputName: "failed.mp4",
		Format:     "mp4",
	}, nil)
	if err == nil {
		t.Fatal("Download() unexpectedly succeeded")
	}
	message := err.Error()
	for _, secret := range []string{requestURL, "secret-token", cookie, "private-agent", "Cookie: super-secret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %q", secret, message)
		}
	}
	for _, entry := range mustReadDir(t, dir) {
		if strings.HasPrefix(entry.Name(), ".riflo-") {
			t.Fatalf("temporary file left after failure: %s", entry.Name())
		}
	}
}

func TestScopedCookieArgUsesSourceDomainAndIsNotAHeader(t *testing.T) {
	request, err := normalizeRequestWithOrigin(
		"https://media.example.test/path/master.m3u8?token=private",
		"https://watch.example.test/video?id=7",
		"https://watch.example.test",
		"riflo-test-agent",
		"session=secret; theme=dark",
	)
	if err != nil {
		t.Fatalf("normalizeRequestWithOrigin() error = %v", err)
	}
	headers := strings.Join(request.HeaderArg, "\n")
	if strings.Contains(strings.ToLower(headers), "cookie:") || strings.Contains(headers, "session=secret") {
		t.Fatalf("Cookie leaked into generic headers: %q", headers)
	}
	cookies := strings.Join(request.CookieArg, "\n")
	for _, want := range []string{
		"session=secret; path=/; domain=media.example.test;",
		"theme=dark; path=/; domain=media.example.test;",
	} {
		if !strings.Contains(cookies, want) {
			t.Fatalf("scoped cookie args missing %q: %q", want, cookies)
		}
	}
}

func TestRequestHeadersRejectCRLF(t *testing.T) {
	_, err := makeHeaderArg("ok\nInjected: yes", "", "")
	if err == nil {
		t.Fatal("makeHeaderArg accepted CRLF injection")
	}
}

func TestOriginHeaderIsNormalizedAndRejectsPageURLs(t *testing.T) {
	request, err := normalizeRequestWithOrigin(
		"https://media.example.test/master.m3u8",
		"",
		"https://watch.example.test/",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("normalizeRequestWithOrigin() error = %v", err)
	}
	if request.Origin != "https://watch.example.test" {
		t.Fatalf("Origin = %q", request.Origin)
	}
	if _, err := normalizeRequestWithOrigin(
		"https://media.example.test/master.m3u8",
		"",
		"https://watch.example.test/video?id=private",
		"",
		"",
	); err == nil {
		t.Fatal("normalizeRequestWithOrigin accepted a page URL as Origin")
	}
}

func TestDownloadResolvesSelectedVariantWithTransientOrigin(t *testing.T) {
	var masterRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/master.m3u8" {
			http.NotFound(w, r)
			return
		}
		masterRequests.Add(1)
		if r.Header.Get("Origin") != "https://watch.example.test" {
			t.Errorf("Origin = %q", r.Header.Get("Origin"))
		}
		if r.Header.Get("Referer") != "https://watch.example.test/video" {
			t.Errorf("Referer = %q", r.Header.Get("Referer"))
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100,RESOLUTION=640x360\n"+
			"/variants/low.m3u8?sig=variant-secret\n")
	}))
	defer server.Close()

	argsPath := filepath.Join(t.TempDir(), "ffmpeg-args")
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"duration\":\"1.0\"}}'")
	ffmpeg := fakeExecutable(t, fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\nfor arg do last=\"$arg\"; done\nprintf 'progress=end\\n'\nprintf media > \"$last\"", shellQuote(argsPath)))
	dir := t.TempDir()
	index := 0
	runner := NewRunner(probe, ffmpeg)
	result, err := runner.Download(context.Background(), "task", app.CreateTaskRequest{
		URL:             server.URL + "/master.m3u8?token=master-secret",
		Referer:         "https://watch.example.test/video",
		Origin:          "https://watch.example.test",
		OutputDir:       dir,
		OutputName:      "selected.mp4",
		Format:          "mp4",
		HLSVariantIndex: &index,
	}, nil)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if result.OutputPath != filepath.Join(dir, "selected.mp4") || masterRequests.Load() != 1 {
		t.Fatalf("result=%+v master requests=%d", result, masterRequests.Load())
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "/variants/low.m3u8?sig=variant-secret") {
		t.Fatalf("ffmpeg did not receive resolved variant URL: %q", args)
	}
	if !strings.Contains(string(args), "Origin: https://watch.example.test") {
		t.Fatalf("ffmpeg did not receive transient Origin: %q", args)
	}
	if strings.Contains(result.OutputPath, "master-secret") {
		t.Fatal("signed source query entered result")
	}
}

func TestDownloadVariantUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100,RESOLUTION=640x360\nlow.m3u8\n")
	}))
	defer server.Close()
	index := 1
	runner := NewRunner("missing-ffprobe", "missing-ffmpeg")
	_, err := runner.Download(context.Background(), "task", app.CreateTaskRequest{
		URL: server.URL + "/master.m3u8", OutputDir: t.TempDir(), OutputName: "missing.mp4", Format: "mp4",
		HLSVariantIndex: &index,
	}, nil)
	if !errors.Is(err, ErrVariantUnavailable) {
		t.Fatalf("Download() error = %v, want ErrVariantUnavailable", err)
	}
}

func TestInspectHLSRateLimitIsBoundedAndClassified(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "ffprobe-ran")
	probe := fakeExecutable(t, fmt.Sprintf("printf x > %s", shellQuote(marker)))
	runner := NewRunner(probe, "missing-ffmpeg")
	_, err := runner.Inspect(context.Background(), app.InspectRequest{URL: server.URL + "/master.m3u8"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Inspect() error = %v, want ErrRateLimited", err)
	}
	if requests.Load() != hlsRequestAttempts {
		t.Fatalf("request attempts = %d, want %d", requests.Load(), hlsRequestAttempts)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ffprobe ran after rate limit, stat error = %v", statErr)
	}
}

func TestInspectHLSRetriesServerErrorsButNotClientErrors(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		status       int
		wantAttempts int32
	}{
		{name: "server error", status: http.StatusServiceUnavailable, wantAttempts: hlsRequestAttempts},
		{name: "bad request", status: http.StatusBadRequest, wantAttempts: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(testCase.status)
			}))
			defer server.Close()

			runner := NewRunner("missing-ffprobe", "missing-ffmpeg")
			_, err := runner.Inspect(context.Background(), app.InspectRequest{URL: server.URL + "/master.m3u8"})
			if !errors.Is(err, ErrHTTPStatus) {
				t.Fatalf("Inspect() error = %v, want ErrHTTPStatus", err)
			}
			if requests.Load() != testCase.wantAttempts {
				t.Fatalf("request attempts = %d, want %d", requests.Load(), testCase.wantAttempts)
			}
		})
	}
}

func TestInspectHLSClassifiesExpiredURLWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()

	runner := NewRunner("missing-ffprobe", "missing-ffmpeg")
	_, err := runner.Inspect(context.Background(), app.InspectRequest{URL: server.URL + "/master.m3u8"})
	if !errors.Is(err, ErrURLExpired) {
		t.Fatalf("Inspect() error = %v, want ErrURLExpired", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("request attempts = %d, want 1", requests.Load())
	}
}

func TestHTTPRetryArgsAreBounded(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "ffmpeg-args")
	probe := fakeExecutable(t, "printf '%s' '{\"format\":{\"duration\":\"1.0\"}}'")
	ffmpeg := fakeExecutable(t, fmt.Sprintf("printf '%%s\\n' \"$@\" > %s\nfor arg do last=\"$arg\"; done\nprintf 'progress=end\\n'\nprintf media > \"$last\"", shellQuote(argsPath)))
	runner := NewRunner(probe, ffmpeg)
	runner.caps = ffmpegcap.Capabilities{
		Reconnect:               true,
		ReconnectStreamed:       true,
		ReconnectOnNetworkError: true,
		ReconnectOnHTTPError:    true,
		ReconnectDelayMax:       true,
		ReconnectMaxRetries:     true,
		ReconnectDelayTotalMax:  true,
		RespectRetryAfter:       true,
		HLSSegmentMaxRetry:      true,
	}
	runner.capOnce.Do(func() {})
	_, err := runner.Download(context.Background(), "task", app.CreateTaskRequest{
		URL: "https://media.example.test/video.m3u8", OutputDir: t.TempDir(), OutputName: "retry.mp4", Format: "mp4",
	}, nil)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-reconnect", "-reconnect_on_http_error", "429,500,502,503,504", "-reconnect_max_retries", "3", "-reconnect_delay_total_max", "10", "-seg_max_retry"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("ffmpeg args missing %q: %q", want, args)
		}
	}
}

func fakeExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-tool.sh")
	script := "#!/bin/sh\nset -eu\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
