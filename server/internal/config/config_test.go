package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyDiskMountRequiresRegularMarker(t *testing.T) {
	base := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = base
	if err := cfg.VerifyDiskMount(); err == nil {
		t.Fatal("expected missing disk marker to reject startup")
	}

	marker := filepath.Join(base, MountMarkerFile)
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyDiskMount(); err == nil {
		t.Fatal("expected marker directory to be rejected")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyDiskMount(); err != nil {
		t.Fatalf("regular marker should allow a non-/mnt test data directory: %v", err)
	}
	for _, dir := range []string{cfg.DBDir(), cfg.MediaDir(), filepath.Join(cfg.MediaDir(), "stories"), filepath.Join(cfg.MediaDir(), "audio"), cfg.TmpDir(), cfg.BackupDir()} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("expected managed directory %s: info=%v err=%v", dir, info, err)
		}
	}
}

func TestVerifyDiskMountRejectsSymlinkMarker(t *testing.T) {
	base := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = base
	outside := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, MountMarkerFile)); err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyDiskMount(); err == nil {
		t.Fatal("expected symlink marker to be rejected")
	}
}

func TestVerifyDiskMountRejectsTmpSymlinkBeforeCreatingTarget(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, MountMarkerFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "created-by-mkdirall")
	if err := os.Symlink(outside, filepath.Join(base, "tmp")); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.DataDir = base
	if err := cfg.VerifyDiskMount(); err == nil {
		t.Fatal("expected TMPDIR symlink to be rejected before startup")
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("startup followed TMPDIR symlink and created a directory outside dataDir: err=%v", err)
	}
}
