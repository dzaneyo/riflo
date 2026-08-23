package config

import (
	"path/filepath"
	"testing"
)

func TestValidateListenAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
		ok   bool
	}{
		{name: "ipv4", addr: "127.0.0.1:8787", ok: true},
		{name: "localhost", addr: "localhost:8787", ok: true},
		{name: "ipv6", addr: "[::1]:8787", ok: true},
		{name: "ephemeral", addr: "127.0.0.1:0", ok: true},
		{name: "all interfaces", addr: ":8787", ok: false},
		{name: "lan", addr: "192.168.1.10:8787", ok: false},
		{name: "dns", addr: "example.com:8787", ok: false},
		{name: "missing port", addr: "127.0.0.1", ok: false},
		{name: "bad port", addr: "127.0.0.1:nope", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateListenAddr(test.addr)
			if (err == nil) != test.ok {
				t.Fatalf("ValidateListenAddr(%q) error = %v, want ok=%v", test.addr, err, test.ok)
			}
		})
	}
}

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		DataDir:      filepath.Join(root, "data"),
		DBPath:       filepath.Join(root, "database", "riflo.db"),
		DownloadsDir: filepath.Join(root, "downloads"),
		ListenAddr:   "127.0.0.1:0",
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}
	for _, path := range []string{cfg.DataDir, filepath.Dir(cfg.DBPath), cfg.DownloadsDir} {
		if info, err := filepath.Abs(path); err != nil || info == "" {
			t.Fatalf("expected absolute path for %q", path)
		}
	}
}
