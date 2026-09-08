package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/dzaneyo/riflo/internal/config"
	"github.com/dzaneyo/riflo/internal/ffmpegcap"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// RunDoctor performs local-only checks and always returns a report, including
// when one or more checks fail. The command can use report.OK for its exit
// status without losing the readable details.
func RunDoctor(ctx context.Context, cfg config.Config) DoctorReport {
	report := DoctorReport{OK: true}
	add := func(name string, err error, detail string) {
		check := DoctorCheck{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			check.Detail = err.Error()
			report.OK = false
		}
		report.Checks = append(report.Checks, check)
	}

	if err := ctx.Err(); err != nil {
		add("context", err, "")
		return report
	}
	if err := cfg.Validate(); err != nil {
		add("configuration", err, "")
		return report
	}
	if err := cfg.EnsureDirs(); err != nil {
		add("directories", err, "")
	} else {
		add("data directory", checkWritable(cfg.DataDir), cfg.DataDir)
		add("database directory", checkWritable(filepath.Dir(cfg.DBPath)), filepath.Dir(cfg.DBPath))
		add("downloads directory", checkWritable(cfg.DownloadsDir), cfg.DownloadsDir)
	}
	for _, name := range []string{"ffprobe", "ffmpeg"} {
		path, err := exec.LookPath(name)
		if err != nil {
			add(name, fmt.Errorf("%s is not available on PATH", name), "")
			continue
		}
		if info, statErr := os.Stat(path); statErr != nil {
			add(name, statErr, "")
			continue
		} else if info.Mode()&0o111 == 0 {
			add(name, fmt.Errorf("%s is not executable", path), "")
			continue
		}
		add(name, nil, path)
		if name == "ffmpeg" {
			caps, capErr := ffmpegcap.Detect(ctx, path)
			if capErr != nil {
				add("ffmpeg capabilities", capErr, "")
				continue
			}
			add("ffmpeg HTTP retry", nil, capabilityDetail(caps.ReliableHTTP()))
			add("ffmpeg bounded retry", nil, capabilityDetail(caps.ReconnectMaxRetries && caps.ReconnectDelayTotalMax))
			add("ffmpeg Retry-After", nil, capabilityDetail(caps.RespectRetryAfter))
			add("ffmpeg HLS segment retry", nil, capabilityDetail(caps.HLSSegmentMaxRetry))
		}
	}
	return report
}

func capabilityDetail(supported bool) string {
	if supported {
		return "supported"
	}
	return "not supported; riflo will disable this optional optimization"
}

func checkWritable(path string) error {
	file, err := os.CreateTemp(path, ".riflo-doctor-*")
	if err != nil {
		return fmt.Errorf("directory %q is not writable: %w", path, err)
	}
	name := file.Name()
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close writability check in %q: %w", path, closeErr)
	}
	if removeErr := os.Remove(name); removeErr != nil {
		return fmt.Errorf("clean writability check in %q: %w", path, removeErr)
	}
	return nil
}
