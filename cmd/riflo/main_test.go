package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dzaneyo/riflo/internal/media"
)

func TestRunVersionAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "riflo version") {
		t.Fatalf("version output = %q", stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "riflo serve") {
		t.Fatalf("help output = %q", stdout.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"unknown"}, &stdout, &stderr); code == 0 {
		t.Fatal("unknown command unexpectedly succeeded")
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unknown command output = %q", stderr.String())
	}
}

func TestTaskCommandsAcceptServerFlagAfterID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "task/invalid", "--server", "http://127.0.0.1:8787"}, &stdout, &stderr); code != 2 {
		t.Fatalf("task invalid ID exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "task ID is invalid") {
		t.Fatalf("task invalid ID stderr = %q", stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"cancel", "--server", "http://example.test:8787", "task-1"}, &stdout, &stderr); code != 2 {
		t.Fatalf("cancel invalid server exit code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "loopback") {
		t.Fatalf("cancel invalid server stderr = %q", stderr.String())
	}
}

func TestRunInspectWithFakeFFprobe(t *testing.T) {
	toolDir := t.TempDir()
	writeFakeTool(t, toolDir, "ffprobe", `#!/bin/sh
printf '%s' '{"format":{"format_name":"hls","duration":"12.5","size":"100"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":640,"height":360}]}'
`)
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", "https://example.test/video.m3u8?token=secret"}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect exit code = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, `"source_display":"https://example.test/video.m3u8"`) || strings.Contains(output, "token") {
		t.Fatalf("inspect output = %q", output)
	}
}

func TestPrintCommandErrorMapsMediaAccessDenied(t *testing.T) {
	var stderr bytes.Buffer
	if code := printCommandError(&stderr, "inspect", fmt.Errorf("probe: %w", media.ErrAccessDenied)); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if got := stderr.String(); got != "riflo inspect: media server denied access; try adding Referer or Cookie\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestRunDownloadWithFakeMediaTools(t *testing.T) {
	toolDir := t.TempDir()
	writeFakeTool(t, toolDir, "ffprobe", `#!/bin/sh
printf '%s' '{"format":{"format_name":"hls","duration":"1","size":"10"},"streams":[]}'
`)
	writeFakeTool(t, toolDir, "ffmpeg", `#!/bin/sh
out=""
for arg in "$@"; do out="$arg"; done
printf 'out_time_ms=1000000\nprogress=end\n'
printf 'media' > "$out"
`)
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	outputDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"download", "https://example.test/video.m3u8?sig=secret", "--output-dir", outputDir, "--output-name", "video.mp4"}, &stdout, &stderr); code != 0 {
		t.Fatalf("download exit code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"output_path":"`+filepath.Join(outputDir, "video.mp4")+`"`) {
		t.Fatalf("download output = %q", stdout.String())
	}
	if data, err := os.ReadFile(filepath.Join(outputDir, "video.mp4")); err != nil || string(data) != "media" {
		t.Fatalf("downloaded file = %q, err = %v", data, err)
	}
}

func writeFakeTool(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}
