package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"reader/server/internal/config"
)

// CurrentVersion is overridden at build time via -ldflags "-X reader/server/internal/updater.CurrentVersion=vX.Y.Z".
var CurrentVersion = "v0.3.4"

type CheckResult struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	HasUpdate      bool   `json:"has_update"`
	Arch           string `json:"arch"`
	Repo           string `json:"repo"`
	ReleaseURL     string `json:"release_url"`
	AssetName      string `json:"asset_name"`
	AssetURL       string `json:"asset_url"`
	AssetSize      int64  `json:"asset_size"`
	ChecksumURL    string `json:"checksum_url,omitempty"`
	PublishedAt    string `json:"published_at"`
	ReleaseNotes   string `json:"release_notes"`
}

type ApplyResult struct {
	Status          string `json:"status"`
	PreviousVersion string `json:"previous_version"`
	NewVersion      string `json:"new_version"`
	Message         string `json:"message"`
}

type githubRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type Updater struct {
	cfg           *config.Config
	httpClient    *http.Client
	apiBaseURL    string
	SkipSelfTest  bool
}

func New(cfg *config.Config) *Updater {
	return &Updater{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
		apiBaseURL: "https://api.github.com",
	}
}

func ExpectedAssetName() string {
	return fmt.Sprintf("reader-%s-%s", runtime.GOOS, runtime.GOARCH)
}

func applyMirror(rawURL, mirrorPrefix string) string {
	m := strings.TrimSpace(mirrorPrefix)
	if m == "" || rawURL == "" {
		return rawURL
	}
	if !strings.HasSuffix(m, "/") {
		m += "/"
	}
	return m + rawURL
}

// CompareVersions returns true if latest is strictly newer than current.
func CompareVersions(current, latest string) bool {
	cParts, cOK := parseSemver(current)
	lParts, lOK := parseSemver(latest)
	if cOK && lOK {
		for i := 0; i < 3; i++ {
			if lParts[i] > cParts[i] {
				return true
			}
			if lParts[i] < cParts[i] {
				return false
			}
		}
		return false
	}
	cur := strings.TrimSpace(current)
	lat := strings.TrimSpace(latest)
	return lat != "" && lat != cur
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return out, false
	}
	for i := 0; i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func (u *Updater) effectiveRepo() string {
	if u.cfg != nil && strings.TrimSpace(u.cfg.Update.Repo) != "" {
		return strings.TrimSpace(u.cfg.Update.Repo)
	}
	return "houalibaba/Reader"
}

func (u *Updater) effectiveMirror(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	if u.cfg != nil {
		return strings.TrimSpace(u.cfg.Update.MirrorURL)
	}
	return ""
}

func (u *Updater) CheckUpdate(ctx context.Context, mirrorOverride string) (*CheckResult, error) {
	repo := u.effectiveRepo()
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(u.apiBaseURL, "/"), repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Reader-AutoUpdater/1.0")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 GitHub 检查更新失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &CheckResult{
			CurrentVersion: CurrentVersion,
			LatestVersion:  CurrentVersion,
			HasUpdate:      false,
			Arch:           runtime.GOOS + "/" + runtime.GOARCH,
			Repo:           repo,
			ReleaseURL:     fmt.Sprintf("https://github.com/%s/releases", repo),
			ReleaseNotes:   "仓库尚未发布任何 Release 版本，请先在 GitHub 推送代码触发自动编译。",
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub API 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 GitHub Release 响应失败: %w", err)
	}

	mirror := u.effectiveMirror(mirrorOverride)
	wantAsset := ExpectedAssetName()
	var assetURL string
	var assetSize int64
	var checksumURL string

	for _, a := range rel.Assets {
		switch a.Name {
		case wantAsset:
			assetURL = applyMirror(a.BrowserDownloadURL, mirror)
			assetSize = a.Size
		case "checksums.txt":
			checksumURL = applyMirror(a.BrowserDownloadURL, mirror)
		}
	}

	hasUpdate := CompareVersions(CurrentVersion, rel.TagName)
	return &CheckResult{
		CurrentVersion: CurrentVersion,
		LatestVersion:  rel.TagName,
		HasUpdate:      hasUpdate,
		Arch:           runtime.GOOS + "/" + runtime.GOARCH,
		Repo:           repo,
		ReleaseURL:     rel.HTMLURL,
		AssetName:      wantAsset,
		AssetURL:       assetURL,
		AssetSize:      assetSize,
		ChecksumURL:    checksumURL,
		PublishedAt:    rel.PublishedAt,
		ReleaseNotes:   rel.Body,
	}, nil
}

func (u *Updater) ApplyUpdate(ctx context.Context, mirrorOverride string) (*ApplyResult, error) {
	check, err := u.CheckUpdate(ctx, mirrorOverride)
	if err != nil {
		return nil, err
	}
	if !check.HasUpdate {
		return &ApplyResult{
			Status:          "up_to_date",
			PreviousVersion: CurrentVersion,
			NewVersion:      check.LatestVersion,
			Message:         fmt.Sprintf("当前已是最新版本 (%s)，无需升级。", CurrentVersion),
		}, nil
	}
	if check.AssetURL == "" {
		return nil, fmt.Errorf("最新版本 %s 中未找到适配当前架构 (%s) 的编译产物 %s", check.LatestVersion, check.Arch, check.AssetName)
	}

	tmpDir := os.TempDir()
	if u.cfg != nil && u.cfg.TmpDir() != "" {
		tmpDir = u.cfg.TmpDir()
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}

	tmpBin := filepath.Join(tmpDir, fmt.Sprintf("reader-%s.tmp", check.LatestVersion))
	defer func() { _ = os.Remove(tmpBin) }()

	actualSHA, written, err := u.downloadFile(ctx, check.AssetURL, tmpBin)
	if err != nil {
		return nil, fmt.Errorf("下载升级包失败: %w", err)
	}
	if written < 1024*1024 {
		return nil, fmt.Errorf("下载的升级包体积异常 (%d 字节)，已中止替换", written)
	}
	if check.AssetSize > 0 && written != check.AssetSize {
		return nil, fmt.Errorf("升级包下载不完整 (期望 %d 字节，实际 %d 字节)", check.AssetSize, written)
	}

	// Gate 1: Verify ELF executable magic header (\x7fELF) so HTML error pages from mirrors are rejected immediately
	if err := verifyELFHeader(tmpBin); err != nil {
		return nil, fmt.Errorf("升级包二进制格式校验失败（可能镜像返回了错误页面）: %w", err)
	}

	// Gate 2: Verify SHA256 checksum
	if check.ChecksumURL != "" {
		if err := u.verifyChecksum(ctx, check.ChecksumURL, check.AssetName, actualSHA); err != nil {
			return nil, fmt.Errorf("SHA256 校验失败: %w", err)
		}
	}

	if err := os.Chmod(tmpBin, 0o755); err != nil {
		return nil, fmt.Errorf("设置可执行权限失败: %w", err)
	}

	// Gate 3: Pre-Flight Subprocess Execution Check (-self-test) BEFORE touching current binary!
	if !u.SkipSelfTest {
		if err := u.runPreFlightSelfTest(ctx, tmpBin); err != nil {
			return nil, fmt.Errorf("新版本启动自检未通过，已中止升级并保持当前服务正常运行: %w", err)
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("无法定位当前程序路径: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	if err := replaceBinary(exePath, tmpBin); err != nil {
		return nil, fmt.Errorf("替换可执行文件失败: %w", err)
	}

	return &ApplyResult{
		Status:          "restarting",
		PreviousVersion: CurrentVersion,
		NewVersion:      check.LatestVersion,
		Message:         fmt.Sprintf("已通过完整性与启动自检并切换至 %s，守护进程正在重启服务（若启动异常将自动回滚）...", check.LatestVersion),
	}, nil
}

func verifyELFHeader(binPath string) error {
	f, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return err
	}
	if !bytes.Equal(magic[:], []byte{0x7f, 'E', 'L', 'F'}) {
		return fmt.Errorf("文件头不是有效的 Linux ELF 可执行文件 (%x)", magic)
	}
	return nil
}

func (u *Updater) runPreFlightSelfTest(ctx context.Context, binPath string) error {
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cfgPath := "/mnt/sda1/reader/config.json"
	if u.cfg != nil && u.cfg.ConfigPath != "" {
		cfgPath = u.cfg.ConfigPath
	} else if u.cfg != nil && u.cfg.DataDir != "" {
		cfgPath = filepath.Join(u.cfg.DataDir, "config.json")
	}

	cmd := exec.CommandContext(testCtx, binPath, "-self-test", "-config", cfgPath)
	if u.cfg != nil && u.cfg.TmpDir() != "" {
		cmd.Env = append(os.Environ(), "TMPDIR="+u.cfg.TmpDir())
	}
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("执行 %s -self-test 失败 (%v): %s", filepath.Base(binPath), err, outStr)
	}
	if !strings.Contains(outStr, "SELF_TEST_OK") {
		return fmt.Errorf("自检输出异常: %s", outStr)
	}
	return nil
}

// SpawnRestartAndRollbackWatchdog launches an independent background shell that:
// 1. Waits briefly for the HTTP response to flush;
// 2. Restarts the service via /etc/init.d/reader restart (or spawns the binary directly);
// 3. Verifies that the new binary stays alive and responds on /api/v1/health;
// 4. Automatically rolls back to reader.bak and restarts if the new binary crashes or fails health check!
func (u *Updater) SpawnRestartAndRollbackWatchdog() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	cfgPath := "/mnt/sda1/reader/config.json"
	tmpDir := "/tmp"
	port := "8380"
	if u.cfg != nil {
		if u.cfg.ConfigPath != "" {
			cfgPath = u.cfg.ConfigPath
		} else if u.cfg.DataDir != "" {
			cfgPath = filepath.Join(u.cfg.DataDir, "config.json")
		}
		if u.cfg.TmpDir() != "" {
			tmpDir = u.cfg.TmpDir()
		}
		if idx := strings.LastIndex(u.cfg.Listen, ":"); idx >= 0 && idx+1 < len(u.cfg.Listen) {
			port = u.cfg.Listen[idx+1:]
		}
	}

	logFile := filepath.Join(tmpDir, "updater_watchdog.log")
	script := fmt.Sprintf(`
sleep 1
if [ -x /etc/init.d/reader ]; then
    /etc/init.d/reader restart >> %q 2>&1 || /etc/init.d/reader start >> %q 2>&1
else
    kill %d 2>/dev/null || true
    sleep 1
    TMPDIR=%q GOMEMLIMIT=120MiB GOGC=50 %q -config %q >> %q 2>&1 &
fi

OK=0
for i in 1 2 3 4 5 6; do
    sleep 1
    if wget -qO- "http://127.0.0.1:%s/api/v1/health" 2>/dev/null | grep -q '"status":"ok"' || curl -fsS "http://127.0.0.1:%s/api/v1/health" 2>/dev/null | grep -q '"status":"ok"'; then
        OK=1
        break
    fi
done

if [ "$OK" = "0" ] && [ -f "%s.bak" ]; then
    echo "[watchdog] new binary failed health check, rolling back to .bak" >> %q
    cp -f "%s.bak" %q
    chmod +x %q
    if [ -x /etc/init.d/reader ]; then
        /etc/init.d/reader restart >> %q 2>&1 || /etc/init.d/reader start >> %q 2>&1
    else
        TMPDIR=%q GOMEMLIMIT=120MiB GOGC=50 %q -config %q >> %q 2>&1 &
    fi
fi
`, logFile, logFile, os.Getpid(), tmpDir, exePath, cfgPath, logFile, port, port, exePath, logFile, exePath, exePath, exePath, logFile, logFile, tmpDir, exePath, cfgPath, logFile)

	cmd := exec.Command("/bin/sh", "-c", script)
	_ = cmd.Start()
}

func (u *Updater) downloadFile(ctx context.Context, url, destPath string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "Reader-AutoUpdater/1.0")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (u *Updater) verifyChecksum(ctx context.Context, checksumURL, assetName, actualSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Reader-AutoUpdater/1.0")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			expected := strings.ToLower(strings.TrimSpace(fields[0]))
			if expected != strings.ToLower(actualSHA) {
				return fmt.Errorf("期望 %s，实际 %s", expected, actualSHA)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt 中未找到 %s 的校验值", assetName)
}

func replaceBinary(exePath, newBinPath string) error {
	bakPath := exePath + ".bak"
	_ = os.Remove(bakPath)

	// Rename running executable to .bak (safe on Linux; running process keeps open inode).
	if err := os.Rename(exePath, bakPath); err != nil {
		return err
	}

	if err := os.Rename(newBinPath, exePath); err == nil {
		return nil
	}

	// Cross-device fallback: copy into target directory as .new, then rename atomically.
	stagePath := exePath + ".new"
	if err := copyFile(newBinPath, stagePath, 0o755); err != nil {
		_ = os.Rename(bakPath, exePath)
		return err
	}
	if err := os.Rename(stagePath, exePath); err != nil {
		_ = os.Remove(stagePath)
		_ = os.Rename(bakPath, exePath)
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return nil
}
