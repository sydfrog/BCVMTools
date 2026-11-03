package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppDataDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base) // UserConfigDir on Linux
	t.Setenv("AppData", base)         // ... and on Windows
	d, err := appDataDir()
	if err != nil || d != filepath.Join(base, "BCVMTools") {
		t.Fatalf("create: %v %q", err, d)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatal("dir not created")
	}
	if os.Geteuid() != 0 { // root ignores permission bits
		os.Chmod(d, 0o500)
		defer os.Chmod(d, 0o755)
		if _, err := appDataDir(); err == nil {
			t.Fatal("expected a permission error")
		}
	}
	// blocked creation: a file where the folder should be
	b2 := t.TempDir()
	os.WriteFile(filepath.Join(b2, "BCVMTools"), nil, 0o644)
	t.Setenv("XDG_CONFIG_HOME", b2)
	t.Setenv("AppData", b2)
	if _, err := appDataDir(); err == nil {
		t.Fatal("expected an error when the folder cannot be created")
	}
}
