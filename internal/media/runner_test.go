package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dzaneyo/riflo/internal/app"
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

func TestRequestHeadersRejectCRLF(t *testing.T) {
	_, err := makeHeaderArg("ok\nInjected: yes", "", "")
	if err == nil {
		t.Fatal("makeHeaderArg accepted CRLF injection")
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
