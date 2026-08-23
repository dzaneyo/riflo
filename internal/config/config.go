// Package config contains the small set of local configuration values shared
// by the CLI and the HTTP server.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// DefaultListenAddr deliberately binds to loopback. Opening the service on
	// a LAN address requires a separate authentication design and is not a
	// configuration switch in the MVP.
	DefaultListenAddr = "127.0.0.1:8787"
	appDirectory      = "riflo"
	databaseFile      = "riflo.db"
	downloadsDir      = "downloads"
)

// Config describes the paths and address used by one riflo process.
type Config struct {
	DataDir      string
	DBPath       string
	DownloadsDir string
	ListenAddr   string
}

// Defaults returns platform-appropriate paths. os.UserConfigDir maps to
// ~/Library/Application Support on macOS, %AppData% on Windows, and the
// user's XDG config directory on Linux.
func Defaults() (Config, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return Config{}, fmt.Errorf("find user config directory: %w", err)
	}
	dataDir := filepath.Join(base, appDirectory)
	return Config{
		DataDir:      dataDir,
		DBPath:       filepath.Join(dataDir, databaseFile),
		DownloadsDir: filepath.Join(dataDir, downloadsDir),
		ListenAddr:   DefaultListenAddr,
	}, nil
}

// Validate checks values which must be safe before creating directories or
// starting a server. It intentionally does not require the paths to exist.
func (c Config) Validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("data directory is required")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		return fmt.Errorf("database path is required")
	}
	if strings.TrimSpace(c.DownloadsDir) == "" {
		return fmt.Errorf("downloads directory is required")
	}
	if err := ValidateListenAddr(c.ListenAddr); err != nil {
		return err
	}
	return nil
}

// EnsureDirs creates the directories needed by the local process. Database
// paths may point at an alternate directory; that parent is created too.
func (c Config) EnsureDirs() error {
	if err := c.Validate(); err != nil {
		return err
	}
	// DataDir is owned by riflo and can contain the SQLite WAL while the
	// service is running, so keep it private even with a permissive umask.
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("create directory %q: %w", c.DataDir, err)
	}
	if err := os.Chmod(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("secure directory %q: %w", c.DataDir, err)
	}
	// An explicitly overridden DB parent may be shared and pre-existing; do not
	// change its permissions, but create a missing parent privately.
	if err := os.MkdirAll(filepath.Dir(c.DBPath), 0o700); err != nil {
		return fmt.Errorf("create directory %q: %w", filepath.Dir(c.DBPath), err)
	}
	if err := os.MkdirAll(c.DownloadsDir, 0o755); err != nil {
		return fmt.Errorf("create directory %q: %w", c.DownloadsDir, err)
	}
	return nil
}

// ValidateListenAddr accepts only an explicit TCP loopback address. Port 0 is
// permitted for tests and callers that ask the OS to choose an ephemeral port.
func ValidateListenAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("listen address is required")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q (use host:port): %w", addr, err)
	}
	if !IsLoopbackHost(host) {
		return fmt.Errorf("listen address %q is not loopback; use localhost, 127.0.0.1, or ::1", addr)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("invalid listen port %q", port)
	}
	return nil
}

// IsLoopbackHost recognizes the names and IPs explicitly allowed by the
// local-only HTTP boundary. It does not resolve arbitrary DNS names.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.TrimSuffix(host, "."))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
