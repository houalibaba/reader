package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reader/server/internal/config"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"v0.3.0", "v0.3.1", true},
		{"v0.3.0", "v0.4.0", true},
		{"v0.3.10", "v0.3.2", false},
		{"v0.3.1", "v0.3.1", false},
		{"v0.3.2", "v0.3.1", false},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.cur, tc.lat); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %v, want %v", tc.cur, tc.lat, got, tc.want)
		}
	}
}

func TestCheckUpdateMockGitHub(t *testing.T) {
	assetName := ExpectedAssetName()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.3.99",
			"html_url":     "https://github.com/houalibaba/Reader/releases/tag/v0.3.99",
			"body":         "更新 Go 服务与词汇规划脚本",
			"published_at": "2026-11-13T12:00:00Z",
			"assets": []map[string]any{
				{"name": assetName, "size": 1234567, "browser_download_url": "https://example.test/" + assetName},
				{"name": runtimeAssetName, "size": 7654, "browser_download_url": "https://example.test/" + runtimeAssetName},
				{"name": "checksums.txt", "size": 128, "browser_download_url": "https://example.test/checksums.txt"},
			},
		})
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.Update.Repo = "houalibaba/Reader"
	u := New(cfg)
	u.apiBaseURL = srv.URL
	res, err := u.CheckUpdate(context.Background(), "")
	if err != nil {
		t.Fatalf("CheckUpdate error: %v", err)
	}
	if !res.HasUpdate || res.LatestVersion != "v0.3.99" || res.AssetName != assetName {
		t.Fatalf("unexpected CheckResult: %+v", res)
	}
	if res.RuntimeAssetName != runtimeAssetName || res.RuntimeAssetURL != "https://example.test/"+runtimeAssetName || res.RuntimeAssetSize != 7654 || res.ChecksumURL != "https://example.test/checksums.txt" {
		t.Fatalf("runtime package and checksum asset were not discovered: %+v", res)
	}
}

func TestReplaceBinaryAndChecksum(t *testing.T) {
	dir := t.TempDir()
	oldBin := filepath.Join(dir, "reader")
	newBin := filepath.Join(dir, "reader.tmp")
	if err := os.WriteFile(oldBin, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("new-binary-content")
	if err := os.WriteFile(newBin, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(oldBin, newBin); err != nil {
		t.Fatalf("replaceBinary failed: %v", err)
	}
	got, err := os.ReadFile(oldBin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary-content" {
		t.Fatalf("expected updated binary, got %q", string(got))
	}
	if _, err := os.Stat(oldBin + ".bak"); err != nil {
		t.Fatalf("expected backup file .bak to exist: %v", err)
	}
	sum := sha256.Sum256(payload)
	if len(hex.EncodeToString(sum[:])) != 64 {
		t.Fatalf("unexpected sha256 length")
	}
}

func TestVerifyELFHeader(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.html")
	if err := os.WriteFile(badFile, []byte("<html>404 Not Found</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyELFHeader(badFile); err == nil {
		t.Fatalf("expected verifyELFHeader to reject non-ELF file")
	}
	goodFile := filepath.Join(dir, "good.bin")
	if err := os.WriteFile(goodFile, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyELFHeader(goodFile); err != nil {
		t.Fatalf("expected verifyELFHeader to accept ELF header, got: %v", err)
	}
}

func TestRuntimeBundleActivationRollbackAndUserDataPreservation(t *testing.T) {
	base, tmp := makeRuntimeDirs(t)
	userFiles := map[string]string{
		"config.json":                `{"guardian_pin":"private"}`,
		"data/reader.db":             "existing database",
		"media/stories/story.zip":    "existing media",
		"backups/user-backup.sqlite": "existing backup",
	}
	for rel, content := range userFiles {
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	firstFiles := runtimeTestFiles("first")
	if err := activateRuntimeBundle(base, tmp, "v0.3.11", makeRuntimeArchive(t, "v0.3.11", firstFiles)); err != nil {
		t.Fatalf("activate initial runtime: %v", err)
	}
	if got := readRuntimeLink(t, base); got != filepath.Join("releases", "v0.3.11") {
		t.Fatalf("unexpected current runtime: %q", got)
	}
	planner, err := os.ReadFile(filepath.Join(base, "runtime/current/tools/vocab_planner.py"))
	if err != nil || string(planner) != firstFiles["tools/vocab_planner.py"] {
		t.Fatalf("vocab planner not delivered: err=%v content=%q", err, planner)
	}
	for _, rel := range []string{"tools/helper.sh", "tools/subdir/helper.py", "deploy/helpers/reader.sh"} {
		if _, err := os.Stat(filepath.Join(base, "runtime/current", filepath.FromSlash(rel))); err != nil {
			t.Errorf("new runtime file %s was not delivered: %v", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "tools")); err != nil {
		t.Fatalf("compatibility tools link not created: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "runtime/current.previous-none")); err != nil {
		t.Fatalf("initial no-previous rollback marker missing: %v", err)
	}

	secondFiles := runtimeTestFiles("second")
	if err := activateRuntimeBundle(base, tmp, "v0.3.12", makeRuntimeArchive(t, "v0.3.12", secondFiles)); err != nil {
		t.Fatalf("activate runtime update: %v", err)
	}
	if got := readRuntimeLink(t, base); got != filepath.Join("releases", "v0.3.12") {
		t.Fatalf("unexpected updated runtime: %q", got)
	}
	if got, err := os.Readlink(filepath.Join(base, "runtime/current.previous")); err != nil || got != filepath.Join("releases", "v0.3.11") {
		t.Fatalf("previous runtime not retained for rollback: target=%q err=%v", got, err)
	}
	if err := rollbackRuntimeActivation(base); err != nil {
		t.Fatalf("rollback runtime: %v", err)
	}
	if got := readRuntimeLink(t, base); got != filepath.Join("releases", "v0.3.11") {
		t.Fatalf("rollback did not restore prior runtime: %q", got)
	}

	for rel, want := range userFiles {
		got, err := os.ReadFile(filepath.Join(base, rel))
		if err != nil || string(got) != want {
			t.Errorf("user file %s changed: content=%q err=%v", rel, got, err)
		}
	}
}

func TestRuntimeBundleCommitKeepsOldReleaseAndRemovesOnlyRollbackMarker(t *testing.T) {
	base, tmp := makeRuntimeDirs(t)
	if err := activateRuntimeBundle(base, tmp, "v0.3.11", makeRuntimeArchive(t, "v0.3.11", runtimeTestFiles("one"))); err != nil {
		t.Fatal(err)
	}
	if err := activateRuntimeBundle(base, tmp, "v0.3.12", makeRuntimeArchive(t, "v0.3.12", runtimeTestFiles("two"))); err != nil {
		t.Fatal(err)
	}
	if err := CommitRuntimeActivation(base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "runtime/current.previous")); !os.IsNotExist(err) {
		t.Fatalf("rollback pointer should be cleared after health check, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "runtime/releases/v0.3.11/tools/vocab_planner.py")); err != nil {
		t.Fatalf("previous runtime release should be retained: %v", err)
	}
	if got := readRuntimeLink(t, base); got != filepath.Join("releases", "v0.3.12") {
		t.Fatalf("commit changed active runtime: %q", got)
	}
}

func TestLegacyV0310FirstUpgradeFailureRollsBackRuntimePointer(t *testing.T) {
	base, tmp := makeRuntimeDirs(t)
	current := filepath.Join(base, "runtime", "current")
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("v0.3.10 first-upgrade fixture should start without a runtime pointer, err=%v", err)
	}

	// The new binary installs its embedded runtime after the legacy updater has
	// replaced the executable. v0.3.10 only restores the binary, then its old
	// service comes back healthy; it cannot see or restore runtime/current.
	files := runtimeTestFiles("v0.3.11")
	if err := activateRuntimeBundle(base, tmp, "v0.3.11", makeRuntimeArchive(t, "v0.3.11", files)); err != nil {
		t.Fatalf("install first embedded runtime: %v", err)
	}
	if got := readRuntimeLink(t, base); got != filepath.Join("releases", "v0.3.11") {
		t.Fatalf("new runtime was not initially activated: %q", got)
	}
	if !RuntimeActivationPending(base) {
		t.Fatal("first runtime activation must retain a rollback marker until health succeeds")
	}

	legacyService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "v0.3.10"})
	}))
	defer legacyService.Close()
	listen := strings.TrimPrefix(legacyService.URL, "http://")
	if err := RunRuntimeActivationWatchdog(base, listen, "v0.3.11", time.Millisecond); err != nil {
		t.Fatalf("legacy-updater first-upgrade rollback failed: %v", err)
	}
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("v0.3.10 binary rollback must restore the pre-upgrade no-runtime state, current err=%v", err)
	}
	if RuntimeActivationPending(base) {
		t.Fatal("runtime rollback markers should be cleared after restoring first-upgrade state")
	}
}

func TestRuntimeBundleRejectsTamperingTraversalAndWrongVersion(t *testing.T) {
	base, tmp := makeRuntimeDirs(t)
	files := runtimeTestFiles("safe")
	bad := tamperRuntimePayload(t, makeRuntimeArchive(t, "v0.3.12", files), []byte("# planner safe\n"), []byte("# planner evil\n"))
	if err := activateRuntimeBundle(base, tmp, "v0.3.12", bad); err == nil {
		t.Fatal("expected tampered runtime archive to be rejected")
	}
	if _, err := os.Lstat(filepath.Join(base, "runtime/current")); !os.IsNotExist(err) {
		t.Fatalf("tampered runtime must not activate, err=%v", err)
	}
	if err := activateRuntimeBundle(base, tmp, "v0.3.13", makeRuntimeArchive(t, "v0.3.12", files)); err == nil {
		t.Fatal("expected mismatched runtime version to be rejected")
	}
	traversal := map[string]string{
		"reader_menu.sh":          "menu",
		"tools/menu.py":           "menu",
		"tools/story_packager.py": "packager",
		"tools/vocab_planner.py":  "planner",
		"../outside.py":           "escape",
	}
	if err := activateRuntimeBundle(base, tmp, "v0.3.14", makeRuntimeArchive(t, "v0.3.14", traversal)); err == nil {
		t.Fatal("expected path traversal entry to be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(base), "outside.py")); !os.IsNotExist(err) {
		t.Fatalf("archive escaped the external data directory, err=%v", err)
	}
}

func TestUpdateStorageRequiresMarkerAndTemporaryDirectoryInsideDataDisk(t *testing.T) {
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdateStorage(base, tmp); err == nil {
		t.Fatal("expected missing external disk marker to reject update")
	}
	if err := os.WriteFile(filepath.Join(base, ".reader-disk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdateStorage(base, t.TempDir()); err == nil {
		t.Fatal("expected tmp path outside data directory to be rejected")
	}
	if err := validateUpdateStorage(base, tmp); err != nil {
		t.Fatalf("valid mounted-disk layout rejected: %v", err)
	}
}

func TestUpdateStorageRejectsRootVolumeBeforeCreatingTmp(t *testing.T) {
	base, _ := makeRuntimeDirs(t)
	tmp := filepath.Join(base, "tmp-not-created")
	err := validateUpdateStorageAgainstRoot(base, tmp, base, true)
	if err == nil || !strings.Contains(err.Error(), "mounted external disk") {
		t.Fatalf("expected root-volume update to be refused, got %v", err)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("refused update must not create a temporary directory on root volume, err=%v", err)
	}
}

func TestUpdateStorageRejectsTmpSymlinkBeforeFollowingIt(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, ".reader-disk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "created-by-mkdirall")
	tmp := filepath.Join(base, "tmp")
	if err := os.Symlink(outside, tmp); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdateStorageAgainstRoot(base, tmp, "/", false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected tmp symlink to be rejected before mkdir, got %v", err)
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Fatalf("validation followed tmp symlink and created an external directory: err=%v", err)
	}
}

func TestUpdateFreeSpacePreservesConfiguredReserve(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.MinFreeDiskMB = 1 << 62
	if err := checkUpdateFreeSpace(cfg, 2<<20, 1<<20); err == nil || !strings.Contains(err.Error(), "空间不足") {
		t.Fatalf("expected low free-space check to reject update, got %v", err)
	}
	if err := checkUpdateFreeSpace(cfg, -1, 1); err == nil {
		t.Fatal("expected invalid release asset sizes to be rejected")
	}
}

func TestEnsureEmbeddedRuntimeBootstrap(t *testing.T) {
	base, tmp := makeRuntimeDirs(t)
	if err := EnsureEmbeddedRuntime(base, tmp, CurrentVersion); err != nil {
		t.Fatalf("embedded runtime bootstrap failed: %v", err)
	}
	if got := readRuntimeLink(t, base); got != filepath.Join("releases", CurrentVersion) {
		t.Fatalf("embedded runtime not activated: %q", got)
	}
	planner, err := os.ReadFile(filepath.Join(base, "runtime/current/tools/vocab_planner.py"))
	if err != nil || !strings.Contains(string(planner), "generate_vocabulary_plan") {
		t.Fatalf("embedded vocab_planner.py missing or invalid: err=%v", err)
	}
}

func TestHealthCheckURLUsesConfiguredLANAddress(t *testing.T) {
	if got, want := HealthCheckURL("192.168.100.1:8380"), "http://192.168.100.1:8380/api/v1/health"; got != want {
		t.Fatalf("HealthCheckURL() = %q, want %q", got, want)
	}
	if got, want := HealthCheckURL(":8380"), "http://127.0.0.1:8380/api/v1/health"; got != want {
		t.Fatalf("HealthCheckURL() = %q, want %q", got, want)
	}
	if got, ok := LoopbackHealthAddress("192.168.100.1:8380"); !ok || got != "127.0.0.1:8380" {
		t.Fatalf("LAN-only listener should enable a loopback compatibility probe, got %q enabled=%v", got, ok)
	}
	if _, ok := LoopbackHealthAddress(":8380"); ok {
		t.Fatal("wildcard listener should not create a duplicate loopback listener")
	}
	if _, ok := LoopbackHealthAddress("127.0.0.1:8380"); ok {
		t.Fatal("loopback listener should not create a duplicate listener")
	}
}

func TestWatchdogScriptUsesCorrectHealthURLAndIsValidShell(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Listen = "192.168.100.1:8380"
	cfg.ConfigPath = filepath.Join(cfg.DataDir, "config.json")
	script := buildWatchdogScript(cfg, filepath.Join(cfg.DataDir, "reader"), "v0.3.12", 4242)
	if !strings.Contains(script, "http://192.168.100.1:8380/api/v1/health") {
		t.Fatal("watchdog script does not probe the configured LAN address")
	}
	if strings.Contains(script, "%!") {
		t.Fatalf("watchdog script has an unexpanded format placeholder: %s", script)
	}
	if !strings.Contains(script, "rollback_runtime") || !strings.Contains(script, "current.previous") {
		t.Fatal("watchdog script is missing runtime rollback logic")
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("watchdog script is not valid POSIX shell: %v: %s", err, out)
	}
}

func makeRuntimeDirs(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".reader-disk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return base, tmp
}

func runtimeTestFiles(label string) map[string]string {
	return map[string]string{
		"reader_menu.sh":                "#!/bin/sh\n# " + label + "\n",
		"tools/menu.py":                 "# menu " + label + "\n",
		"tools/story_packager.py":       "# packager " + label + "\n",
		"tools/vocab_planner.py":        "# planner " + label + "\n",
		"tools/build_runtime_bundle.py": "# builder " + label + "\n",
		"tools/helper.sh":               "#!/bin/sh\n# helper " + label + "\n",
		"tools/subdir/helper.py":        "# nested helper " + label + "\n",
		"deploy/config.json":            "{}\n",
		"deploy/deploy_to_istoreos.sh":  "#!/bin/sh\n# deploy " + label + "\n",
		"deploy/reader.init":            "#!/bin/sh\n# init " + label + "\n",
		"deploy/99-reader-hotplug":      "#!/bin/sh\n# hotplug " + label + "\n",
		"deploy/helpers/reader.sh":      "#!/bin/sh\n# deploy helper " + label + "\n",
	}
}

func makeRuntimeArchive(t *testing.T, version string, files map[string]string) []byte {
	t.Helper()
	type manifestEntry struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	manifest := struct {
		FormatVersion int             `json:"format_version"`
		Version       string          `json:"version"`
		Commit        string          `json:"commit"`
		Files         []manifestEntry `json:"files"`
	}{FormatVersion: 1, Version: version, Commit: "test"}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	// Stable order makes fixture archives reproducible.
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if paths[j] < paths[i] {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}
	for _, path := range paths {
		payload := []byte(files[path])
		sum := sha256.Sum256(payload)
		manifest.Files = append(manifest.Files, manifestEntry{Path: path, Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])})
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gz)
	write := func(name string, payload []byte) {
		t.Helper()
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if name == "reader_menu.sh" {
			header.Mode = 0o755
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	write("runtime-manifest.json", manifestBytes)
	for _, path := range paths {
		write(path, []byte(files[path]))
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func readRuntimeLink(t *testing.T, base string) string {
	t.Helper()
	link, err := os.Readlink(filepath.Join(base, "runtime/current"))
	if err != nil {
		t.Fatalf("read runtime/current: %v", err)
	}
	return link
}

func tamperRuntimePayload(t *testing.T, archive, old, replacement []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	_ = gz.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, old) {
		t.Fatalf("test payload %q not found in archive", old)
	}
	plain = bytes.Replace(plain, old, replacement, 1)
	var result bytes.Buffer
	out := gzip.NewWriter(&result)
	if _, err := out.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestRuntimeManifestHasNoUnexpectedFiles(t *testing.T) {
	base, tmp := makeRuntimeDirs(t)
	files := runtimeTestFiles("fixture")
	files["misc/escape.py"] = "not allowed"
	err := activateRuntimeBundle(base, tmp, "v0.3.11", makeRuntimeArchive(t, "v0.3.11", files))
	if err == nil || !strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("expected unapproved path rejection, got %v", err)
	}
}
