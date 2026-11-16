package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

const (
	runtimeBundleMaxBytes = 32 << 20
	runtimeFilesMaxBytes  = 64 << 20
	runtimeFileMaxBytes   = 16 << 20
	runtimeFilesMaxCount  = 256
)

var runtimeInstallMu sync.Mutex
var safeRuntimeVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

type runtimeManifest struct {
	FormatVersion int                   `json:"format_version"`
	Version       string                `json:"version"`
	Commit        string                `json:"commit"`
	Files         []runtimeManifestFile `json:"files"`
}

type runtimeManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// EnsureEmbeddedRuntime bootstraps/upgrades Python and deployment-side runtime
// assets embedded in the binary. It is intentionally called only after the
// external-disk marker has been checked by the application.
func EnsureEmbeddedRuntime(dataDir, tmpDir, version string) error {
	payload, err := base64.StdEncoding.DecodeString(embeddedRuntimeBundleBase64)
	if err != nil {
		return fmt.Errorf("decode embedded runtime bundle: %w", err)
	}
	return activateRuntimeBundle(dataDir, tmpDir, version, payload)
}

// validateUpdateStorage prevents updater payloads or logs from falling back to
// router flash or RAM-backed /tmp. On the iStoreOS /mnt layout, the data disk
// must be a distinct mounted filesystem from root.
func validateUpdateStorage(dataDir, tmpDir string) error {
	return validateUpdateStorageAgainstRoot(dataDir, tmpDir, "/", strings.HasPrefix(filepath.Clean(dataDir), "/mnt/"))
}

func validateUpdateStorageAgainstRoot(dataDir, tmpDir, root string, requireExternal bool) error {
	if !filepath.IsAbs(dataDir) || !filepath.IsAbs(tmpDir) {
		return fmt.Errorf("update paths must be absolute (data_dir=%q tmp_dir=%q)", dataDir, tmpDir)
	}
	base := filepath.Clean(dataDir)
	tmp := filepath.Clean(tmpDir)
	if base == "/" || tmp == "/tmp" || tmp == "/" {
		return fmt.Errorf("refusing unsafe update storage path (data_dir=%q tmp_dir=%q)", base, tmp)
	}
	rel, err := filepath.Rel(base, tmp)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("temporary update directory %q must be inside external data directory %q", tmp, base)
	}
	info, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("stat external data directory %s: %w", base, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("external data directory %s must be a real directory", base)
	}
	marker, err := os.Lstat(filepath.Join(base, ".reader-disk"))
	if err != nil {
		return fmt.Errorf("external disk marker is required before updating: %w", err)
	}
	if !marker.Mode().IsRegular() {
		return fmt.Errorf("external disk marker %s must be a regular file", filepath.Join(base, ".reader-disk"))
	}
	baseDev, err := deviceID(base)
	if err != nil {
		return err
	}
	if requireExternal {
		rootDev, err := deviceID(root)
		if err != nil {
			return err
		}
		if baseDev == rootDev {
			return fmt.Errorf("%s is not on a mounted external disk; refusing to write update files to router flash", base)
		}
	}
	if err := ensureNoSymlinkPath(base, tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fmt.Errorf("create external-disk temporary directory: %w", err)
	}
	tmpInfo, err := os.Lstat(tmp)
	if err != nil {
		return fmt.Errorf("stat external-disk temporary directory %s: %w", tmp, err)
	}
	if !tmpInfo.IsDir() || tmpInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("temporary update directory %s must be a real directory", tmp)
	}
	tmpDev, err := deviceID(tmp)
	if err != nil {
		return err
	}
	if baseDev != tmpDev {
		return fmt.Errorf("external data and temporary directories are on different filesystems")
	}
	return nil
}

func ensureNoSymlinkPath(base, target string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("temporary update directory %q must be inside external data directory %q", target, base)
	}
	current := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect update path component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in temporary update path at %s", current)
		}
		if current != target && !info.IsDir() {
			return fmt.Errorf("non-directory in temporary update path at %s", current)
		}
	}
	return nil
}

func deviceID(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat filesystem for %s: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot verify filesystem for %s", path)
	}
	return uint64(stat.Dev), nil
}

func activateRuntimeBundle(dataDir, tmpDir, version string, archive []byte) error {
	runtimeInstallMu.Lock()
	defer runtimeInstallMu.Unlock()
	if !safeRuntimeVersion.MatchString(version) || strings.Contains(version, "..") {
		return fmt.Errorf("unsafe runtime version %q", version)
	}
	if err := validateUpdateStorage(dataDir, tmpDir); err != nil {
		return err
	}
	if len(archive) == 0 || len(archive) > runtimeBundleMaxBytes {
		return fmt.Errorf("runtime bundle size is invalid (%d bytes)", len(archive))
	}
	stage, err := os.MkdirTemp(tmpDir, "reader-runtime-stage-")
	if err != nil {
		return fmt.Errorf("create runtime staging directory on external disk: %w", err)
	}
	defer os.RemoveAll(stage)

	manifest, files, err := extractAndVerifyRuntime(archive, stage, version)
	if err != nil {
		return err
	}
	_ = manifest

	runtimeRoot := filepath.Join(dataDir, "runtime")
	releasesDir := filepath.Join(runtimeRoot, "releases")
	if err := ensureRealDirectory(runtimeRoot); err != nil {
		return err
	}
	if err := ensureRealDirectory(releasesDir); err != nil {
		return err
	}
	target := filepath.Join(releasesDir, version)
	if _, err := os.Lstat(target); err == nil {
		if err := verifyInstalledRuntime(target, version, files); err != nil {
			return fmt.Errorf("existing runtime release %s is not identical to the verified bundle: %w", version, err)
		}
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
		stage = ""
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect runtime release directory: %w", err)
	} else {
		if err := os.Rename(stage, target); err != nil {
			return fmt.Errorf("install runtime release on external disk: %w", err)
		}
		stage = ""
	}

	current := filepath.Join(runtimeRoot, "current")
	if existing, err := os.Readlink(current); err == nil {
		if existing == filepath.Join("releases", version) {
			return ensureCompatibilityLinks(dataDir)
		}
		if !validRuntimeLinkTarget(existing) {
			return fmt.Errorf("refusing to replace unexpected runtime/current target %q", existing)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect active runtime link: %w", err)
	} else if info, statErr := os.Lstat(current); statErr == nil || !os.IsNotExist(statErr) {
		if statErr != nil {
			return statErr
		}
		return fmt.Errorf("refusing to replace non-symlink runtime/current (%s)", info.Mode())
	}

	previous := filepath.Join(runtimeRoot, "current.previous")
	previousNone := filepath.Join(runtimeRoot, "current.previous-none")
	_ = os.Remove(previous)
	_ = os.Remove(previousNone)
	if oldTarget, err := os.Readlink(current); err == nil {
		if err := os.Symlink(oldTarget, previous); err != nil {
			return fmt.Errorf("record previous runtime for rollback: %w", err)
		}
	} else if os.IsNotExist(err) {
		if err := os.WriteFile(previousNone, []byte("no previous runtime\n"), 0o600); err != nil {
			return fmt.Errorf("record initial runtime state for rollback: %w", err)
		}
	} else {
		return fmt.Errorf("read previous runtime link: %w", err)
	}

	newLink := filepath.Join(runtimeRoot, "current.new")
	_ = os.Remove(newLink)
	if err := os.Symlink(filepath.Join("releases", version), newLink); err != nil {
		return fmt.Errorf("stage active runtime link: %w", err)
	}
	if err := os.Rename(newLink, current); err != nil {
		_ = os.Remove(newLink)
		_ = os.Remove(previous)
		_ = os.Remove(previousNone)
		return fmt.Errorf("atomically activate runtime release: %w", err)
	}
	if err := ensureCompatibilityLinks(dataDir); err != nil {
		_ = rollbackRuntimeActivation(dataDir)
		return err
	}
	return nil
}

func extractAndVerifyRuntime(archive []byte, stage, expectedVersion string) (runtimeManifest, map[string][]byte, error) {
	var manifest runtimeManifest
	contents := make(map[string][]byte)
	seen := make(map[string]bool)
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return manifest, nil, fmt.Errorf("open runtime archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	var manifestBytes []byte
	for count := 0; ; count++ {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest, nil, fmt.Errorf("read runtime archive: %w", err)
		}
		if count >= runtimeFilesMaxCount+1 {
			return manifest, nil, fmt.Errorf("runtime archive contains too many entries")
		}
		name := header.Name
		if name == "" || filepath.IsAbs(name) || filepath.Clean(filepath.FromSlash(name)) != filepath.FromSlash(name) || strings.Contains(name, "\\") || strings.HasPrefix(name, "../") {
			return manifest, nil, fmt.Errorf("unsafe runtime archive path %q", name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return manifest, nil, fmt.Errorf("runtime archive entry %q is not a regular file", name)
		}
		if seen[name] {
			return manifest, nil, fmt.Errorf("duplicate runtime archive entry %q", name)
		}
		seen[name] = true
		if header.Size < 0 || header.Size > runtimeFileMaxBytes {
			return manifest, nil, fmt.Errorf("runtime archive entry %q has invalid size %d", name, header.Size)
		}
		total += header.Size
		if total > runtimeFilesMaxBytes {
			return manifest, nil, fmt.Errorf("runtime archive expands beyond the allowed size")
		}
		if name != "runtime-manifest.json" && !allowedRuntimePath(name) {
			return manifest, nil, fmt.Errorf("runtime archive contains unapproved path %q", name)
		}
		payload, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if err != nil {
			return manifest, nil, fmt.Errorf("read runtime archive entry %q: %w", name, err)
		}
		if int64(len(payload)) != header.Size {
			return manifest, nil, fmt.Errorf("runtime archive entry %q is truncated", name)
		}
		if name == "runtime-manifest.json" {
			if len(payload) > 1<<20 {
				return manifest, nil, fmt.Errorf("runtime manifest is too large")
			}
			manifestBytes = payload
		} else {
			contents[name] = payload
		}
	}
	if len(manifestBytes) == 0 {
		return manifest, nil, fmt.Errorf("runtime archive is missing runtime-manifest.json")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return manifest, nil, fmt.Errorf("parse runtime manifest: %w", err)
	}
	if manifest.FormatVersion != 1 || manifest.Version != expectedVersion {
		return manifest, nil, fmt.Errorf("runtime manifest version mismatch (format=%d version=%q expected=%q)", manifest.FormatVersion, manifest.Version, expectedVersion)
	}
	if len(manifest.Files) != len(contents) {
		return manifest, nil, fmt.Errorf("runtime manifest lists %d files but archive contains %d", len(manifest.Files), len(contents))
	}
	manifestSeen := make(map[string]bool)
	for _, entry := range manifest.Files {
		if !allowedRuntimePath(entry.Path) || manifestSeen[entry.Path] {
			return manifest, nil, fmt.Errorf("runtime manifest has invalid or duplicate path %q", entry.Path)
		}
		manifestSeen[entry.Path] = true
		payload, ok := contents[entry.Path]
		if !ok || int64(len(payload)) != entry.Size {
			return manifest, nil, fmt.Errorf("runtime file %q is missing or has the wrong size", entry.Path)
		}
		sum := sha256.Sum256(payload)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), entry.SHA256) {
			return manifest, nil, fmt.Errorf("runtime file %q failed its SHA256 verification", entry.Path)
		}
	}
	for _, required := range []string{
		"reader_menu.sh", "tools/menu.py", "tools/story_packager.py", "tools/vocab_planner.py",
		"deploy/deploy_to_istoreos.sh", "deploy/reader.init", "deploy/99-reader-hotplug", "deploy/config.json",
	} {
		if _, ok := contents[required]; !ok {
			return manifest, nil, fmt.Errorf("runtime archive is missing required file %q", required)
		}
	}
	for name, payload := range contents {
		destination := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return manifest, nil, fmt.Errorf("create staged runtime path: %w", err)
		}
		mode := os.FileMode(0o644)
		if name == "reader_menu.sh" || strings.HasSuffix(name, ".sh") || name == "deploy/reader.init" || name == "deploy/99-reader-hotplug" {
			mode = 0o755
		}
		if err := os.WriteFile(destination, payload, mode); err != nil {
			return manifest, nil, fmt.Errorf("stage runtime file %q: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "runtime-manifest.json"), manifestBytes, 0o644); err != nil {
		return manifest, nil, fmt.Errorf("stage runtime manifest: %w", err)
	}
	return manifest, contents, nil
}

func allowedRuntimePath(name string) bool {
	path := filepath.FromSlash(name)
	if name == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(name, "\\") {
		return false
	}
	if name == "reader_menu.sh" {
		return true
	}
	if strings.HasPrefix(name, "tools/") && (strings.HasSuffix(name, ".py") || strings.HasSuffix(name, ".sh")) {
		return true
	}
	if strings.HasPrefix(name, "deploy/") && len(strings.TrimPrefix(name, "deploy/")) > 0 {
		return true
	}
	return false
}

func verifyInstalledRuntime(root, version string, expected map[string][]byte) error {
	manifestData, err := os.ReadFile(filepath.Join(root, "runtime-manifest.json"))
	if err != nil {
		return err
	}
	var manifest runtimeManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return err
	}
	if manifest.FormatVersion != 1 || manifest.Version != version || len(manifest.Files) != len(expected) {
		return fmt.Errorf("installed manifest does not match requested runtime")
	}
	for _, entry := range manifest.Files {
		if !allowedRuntimePath(entry.Path) {
			return fmt.Errorf("installed manifest contains invalid path %q", entry.Path)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return err
		}
		if int64(len(data)) != entry.Size {
			return fmt.Errorf("installed file %q has wrong size", entry.Path)
		}
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), entry.SHA256) {
			return fmt.Errorf("installed file %q has wrong SHA256", entry.Path)
		}
		want, ok := expected[entry.Path]
		if !ok || !bytes.Equal(data, want) {
			return fmt.Errorf("installed file %q differs from verified release", entry.Path)
		}
	}
	return nil
}

func ensureRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", path, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect directory %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-directory or symlink at %s", path)
	}
	return nil
}

func validRuntimeLinkTarget(target string) bool {
	if filepath.IsAbs(target) || filepath.Clean(target) != target || !strings.HasPrefix(target, "releases/") {
		return false
	}
	version := strings.TrimPrefix(target, "releases/")
	return !strings.Contains(version, "/") && safeRuntimeVersion.MatchString(version) && !strings.Contains(version, "..")
}

func ensureCompatibilityLinks(dataDir string) error {
	links := []struct{ name, target string }{
		{"tools", filepath.Join("runtime", "current", "tools")},
		{"reader_menu.sh", filepath.Join("runtime", "current", "reader_menu.sh")},
	}
	for _, link := range links {
		path := filepath.Join(dataDir, link.name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			if err := os.Symlink(link.target, path); err != nil {
				return fmt.Errorf("create compatibility link %s: %w", path, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect compatibility path %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			oldTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if oldTarget == link.target {
				continue
			}
		}
		// Existing user files/directories/links are deliberately left untouched;
		// the versioned payload remains available under runtime/current.
	}
	return nil
}

// CommitRuntimeActivation discards only the pointer rollback marker after the
// newly-started server has passed its local health check. Previous release files
// are retained on the external disk.
func CommitRuntimeActivation(dataDir string) error {
	runtimeRoot := filepath.Join(dataDir, "runtime")
	if err := os.Remove(filepath.Join(runtimeRoot, "current.previous")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Join(runtimeRoot, "current.previous-none")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RuntimeActivationPending reports whether the active runtime pointer still
// has a rollback marker. It is used by the detached bootstrap watchdog because
// the legacy v0.3.10 updater cannot commit or roll back this pointer itself.
func RuntimeActivationPending(dataDir string) bool {
	runtimeRoot := filepath.Join(dataDir, "runtime")
	for _, name := range []string{"current.previous", "current.previous-none"} {
		if _, err := os.Lstat(filepath.Join(runtimeRoot, name)); err == nil {
			return true
		}
	}
	return false
}

// RollbackRuntimeActivation restores the pointer state recorded before the
// pending release was activated. It is exposed for bootstrap fail-safe paths.
func RollbackRuntimeActivation(dataDir string) error {
	return rollbackRuntimeActivation(dataDir)
}

func rollbackRuntimeActivation(dataDir string) error {
	runtimeRoot := filepath.Join(dataDir, "runtime")
	current := filepath.Join(runtimeRoot, "current")
	previous := filepath.Join(runtimeRoot, "current.previous")
	previousNone := filepath.Join(runtimeRoot, "current.previous-none")
	if target, err := os.Readlink(previous); err == nil {
		if !validRuntimeLinkTarget(target) {
			return fmt.Errorf("refusing unsafe previous runtime target %q", target)
		}
		newLink := filepath.Join(runtimeRoot, "current.rollback")
		_ = os.Remove(newLink)
		if err := os.Symlink(target, newLink); err != nil {
			return err
		}
		if err := os.Rename(newLink, current); err != nil {
			_ = os.Remove(newLink)
			return err
		}
		_ = os.Remove(previous)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(previousNone); err == nil {
		if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Remove(previousNone)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
