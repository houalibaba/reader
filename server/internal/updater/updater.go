package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"reader/server/internal/config"
)

// CurrentVersion is overridden at build time via -ldflags.
var CurrentVersion = "v0.3.11"

var localHealthClient = &http.Client{
	Timeout:   2 * time.Second,
	Transport: &http.Transport{Proxy: nil},
}

const (
	runtimeAssetName                     = "reader-runtime.tar.gz"
	RuntimeActivationRollbackGracePeriod = 30 * time.Second
	maxRuntimeRollbackGracePeriod        = 5 * time.Minute
)

type CheckResult struct {
	CurrentVersion   string `json:"current_version"`
	LatestVersion    string `json:"latest_version"`
	HasUpdate        bool   `json:"has_update"`
	Arch             string `json:"arch"`
	Repo             string `json:"repo"`
	ReleaseURL       string `json:"release_url"`
	AssetName        string `json:"asset_name"`
	AssetURL         string `json:"asset_url"`
	AssetSize        int64  `json:"asset_size"`
	RuntimeAssetName string `json:"runtime_asset_name"`
	RuntimeAssetURL  string `json:"runtime_asset_url"`
	RuntimeAssetSize int64  `json:"runtime_asset_size"`
	ChecksumURL      string `json:"checksum_url,omitempty"`
	PublishedAt      string `json:"published_at"`
	ReleaseNotes     string `json:"release_notes"`
}

type ApplyResult struct {
	Status          string `json:"status"`
	ExePath         string `json:"-"`
	RuntimeRoot     string `json:"-"`
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
	cfg          *config.Config
	httpClient   *http.Client
	apiBaseURL   string
	SkipSelfTest bool
	applyMu      sync.Mutex
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
	var assetURL, runtimeURL, checksumURL string
	var assetSize, runtimeSize int64
	for _, a := range rel.Assets {
		switch a.Name {
		case wantAsset:
			assetURL = applyMirror(a.BrowserDownloadURL, mirror)
			assetSize = a.Size
		case runtimeAssetName:
			runtimeURL = applyMirror(a.BrowserDownloadURL, mirror)
			runtimeSize = a.Size
		case "checksums.txt":
			checksumURL = applyMirror(a.BrowserDownloadURL, mirror)
		}
	}

	return &CheckResult{
		CurrentVersion:   CurrentVersion,
		LatestVersion:    rel.TagName,
		HasUpdate:        CompareVersions(CurrentVersion, rel.TagName),
		Arch:             runtime.GOOS + "/" + runtime.GOARCH,
		Repo:             repo,
		ReleaseURL:       rel.HTMLURL,
		AssetName:        wantAsset,
		AssetURL:         assetURL,
		AssetSize:        assetSize,
		RuntimeAssetName: runtimeAssetName,
		RuntimeAssetURL:  runtimeURL,
		RuntimeAssetSize: runtimeSize,
		ChecksumURL:      checksumURL,
		PublishedAt:      rel.PublishedAt,
		ReleaseNotes:     rel.Body,
	}, nil
}

func (u *Updater) ApplyUpdate(ctx context.Context, mirrorOverride string) (*ApplyResult, error) {
	u.applyMu.Lock()
	defer u.applyMu.Unlock()

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
	if check.RuntimeAssetURL == "" {
		return nil, fmt.Errorf("最新版本 %s 缺少运行时脚本包 %s；为避免只更新二进制，已中止升级", check.LatestVersion, runtimeAssetName)
	}
	if check.ChecksumURL == "" {
		return nil, fmt.Errorf("最新版本 %s 缺少 checksums.txt；已中止升级", check.LatestVersion)
	}
	if u.cfg == nil {
		return nil, fmt.Errorf("升级器缺少外接盘配置，拒绝使用系统临时目录")
	}
	tmpDir := u.cfg.TmpDir()
	if err := validateUpdateStorage(u.cfg.DataDir, tmpDir); err != nil {
		return nil, fmt.Errorf("更新存储校验失败: %w", err)
	}
	if err := checkUpdateFreeSpace(u.cfg, check.AssetSize, check.RuntimeAssetSize); err != nil {
		return nil, err
	}
	workDir, err := os.MkdirTemp(tmpDir, "reader-update-")
	if err != nil {
		return nil, fmt.Errorf("在外接盘创建升级暂存目录失败: %w", err)
	}
	defer os.RemoveAll(workDir)

	tmpBin := filepath.Join(workDir, check.AssetName)
	binarySHA, binarySize, err := u.downloadFile(ctx, check.AssetURL, tmpBin, 512<<20)
	if err != nil {
		return nil, fmt.Errorf("下载升级二进制失败: %w", err)
	}
	if binarySize < 1024*1024 {
		return nil, fmt.Errorf("下载的升级二进制体积异常 (%d 字节)，已中止替换", binarySize)
	}
	if check.AssetSize > 0 && binarySize != check.AssetSize {
		return nil, fmt.Errorf("升级二进制下载不完整 (期望 %d 字节，实际 %d 字节)", check.AssetSize, binarySize)
	}
	if err := verifyELFHeader(tmpBin); err != nil {
		return nil, fmt.Errorf("升级包二进制格式校验失败（可能镜像返回了错误页面）: %w", err)
	}
	if err := u.verifyChecksum(ctx, check.ChecksumURL, check.AssetName, binarySHA); err != nil {
		return nil, fmt.Errorf("升级二进制 SHA256 校验失败: %w", err)
	}

	runtimePath := filepath.Join(workDir, runtimeAssetName)
	runtimeSHA, runtimeSize, err := u.downloadFile(ctx, check.RuntimeAssetURL, runtimePath, runtimeBundleMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("下载运行时脚本包失败: %w", err)
	}
	if check.RuntimeAssetSize > 0 && runtimeSize != check.RuntimeAssetSize {
		return nil, fmt.Errorf("运行时脚本包下载不完整 (期望 %d 字节，实际 %d 字节)", check.RuntimeAssetSize, runtimeSize)
	}
	if err := u.verifyChecksum(ctx, check.ChecksumURL, check.RuntimeAssetName, runtimeSHA); err != nil {
		return nil, fmt.Errorf("运行时脚本包 SHA256 校验失败: %w", err)
	}
	runtimeBundle, err := os.ReadFile(runtimePath)
	if err != nil {
		return nil, fmt.Errorf("读取已校验运行时脚本包失败: %w", err)
	}

	if err := os.Chmod(tmpBin, 0o755); err != nil {
		return nil, fmt.Errorf("设置可执行权限失败: %w", err)
	}
	// Pre-flight self-test runs before changing the current binary or runtime pointer.
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
	if err := validateExecutableOnDataDisk(exePath, u.cfg.DataDir); err != nil {
		return nil, err
	}

	if err := activateRuntimeBundle(u.cfg.DataDir, tmpDir, check.LatestVersion, runtimeBundle); err != nil {
		return nil, fmt.Errorf("运行时脚本包校验或外接盘安装失败: %w", err)
	}
	if err := replaceBinary(exePath, tmpBin); err != nil {
		_ = rollbackRuntimeActivation(u.cfg.DataDir)
		return nil, fmt.Errorf("替换可执行文件失败: %w", err)
	}

	return &ApplyResult{
		Status:          "restarting",
		ExePath:         exePath,
		RuntimeRoot:     filepath.Join(u.cfg.DataDir, "runtime"),
		PreviousVersion: CurrentVersion,
		NewVersion:      check.LatestVersion,
		Message:         "二进制与运行时脚本包均已通过完整性校验；服务正在重启，若健康检查失败将自动回滚二进制与脚本版本。",
	}, nil
}

func validateExecutableOnDataDisk(exePath, dataDir string) error {
	rel, err := filepath.Rel(filepath.Clean(dataDir), filepath.Clean(exePath))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("当前 reader 二进制不在外接数据目录内；为避免向路由器闪存写入升级文件，拒绝升级 (%s)", exePath)
	}
	binDev, err := deviceID(exePath)
	if err != nil {
		return err
	}
	dataDev, err := deviceID(dataDir)
	if err != nil {
		return err
	}
	if binDev != dataDev {
		return fmt.Errorf("当前二进制与外接数据盘不在同一文件系统，拒绝写入备份或升级文件")
	}
	return nil
}

func checkUpdateFreeSpace(cfg *config.Config, binarySize, runtimeSize int64) error {
	if cfg == nil || binarySize <= 0 || binarySize > 512<<20 || runtimeSize <= 0 || runtimeSize > runtimeBundleMaxBytes {
		return fmt.Errorf("GitHub Release 的资产大小无效或超过安全上限，已中止升级")
	}
	freeMB, _, err := cfg.CheckFreeDiskMB()
	if err != nil {
		return fmt.Errorf("无法读取外接盘剩余空间: %w", err)
	}
	assetBytes := uint64(binarySize + runtimeSize + (1 << 20) - 1)
	assetMB := assetBytes / (1 << 20)
	neededMB := cfg.MinFreeDiskMB + assetMB
	if freeMB < neededMB {
		return fmt.Errorf("外接盘空间不足：更新需要约 %d MB，并须保留至少 %d MB 空闲空间；当前约 %d MB，已中止升级", assetMB, cfg.MinFreeDiskMB, freeMB)
	}
	return nil
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
	if u.cfg == nil {
		return fmt.Errorf("缺少配置，无法保证自检临时文件位于外接盘")
	}
	cfgPath := u.cfg.ConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(u.cfg.DataDir, "config.json")
	}
	cmd := exec.CommandContext(testCtx, binPath, "-self-test", "-config", cfgPath)
	cmd.Env = append(os.Environ(), "TMPDIR="+u.cfg.TmpDir())
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

// SpawnRestartAndRollbackWatchdog restarts the service, confirms the new
// version on the configured LAN address, then commits the runtime pointer. If
// health fails it restores both reader.bak and the prior runtime symlink.
func (u *Updater) SpawnRestartAndRollbackWatchdog(exePath, expectedVersion string) {
	if u.cfg == nil {
		return
	}
	if exePath == "" {
		if p, err := os.Executable(); err == nil {
			exePath = p
			if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
				exePath = resolved
			}
		}
	}
	exePath = strings.TrimSuffix(exePath, ".bak")
	script := buildWatchdogScript(u.cfg, exePath, expectedVersion, os.Getpid())
	cmd := exec.Command("/bin/sh", "-c", script)
	_ = cmd.Start()
}

func buildWatchdogScript(cfg *config.Config, exePath, expectedVersion string, oldPID int) string {
	cfgPath := cfg.ConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(cfg.DataDir, "config.json")
	}
	tmpDir := cfg.TmpDir()
	logFile := filepath.Join(tmpDir, "updater_watchdog.log")
	runtimeRoot := filepath.Join(cfg.DataDir, "runtime")
	healthCheckURL := HealthCheckURL(cfg.Listen)
	q := shellQuote
	script := fmt.Sprintf(`
RUNTIME_ROOT=%s
rollback_runtime() {
    if [ -L "$RUNTIME_ROOT/current.previous" ]; then
        mv -f "$RUNTIME_ROOT/current.previous" "$RUNTIME_ROOT/current"
    elif [ -f "$RUNTIME_ROOT/current.previous-none" ]; then
        rm -f "$RUNTIME_ROOT/current" "$RUNTIME_ROOT/current.previous-none"
    fi
}
commit_runtime() {
    rm -f "$RUNTIME_ROOT/current.previous" "$RUNTIME_ROOT/current.previous-none"
}
sleep 1
echo "[watchdog] stopping old pid %d and restarting %s (target %s)" >> %s
kill -TERM %d 2>/dev/null || true
sleep 1
kill -KILL %d 2>/dev/null || true
if [ -x /etc/init.d/reader ]; then
    /etc/init.d/reader restart >> %s 2>&1 || /etc/init.d/reader start >> %s 2>&1
fi
sleep 2
if ! (wget -qO- %s 2>/dev/null | grep -q %s || curl -fsS %s 2>/dev/null | grep -q %s); then
    echo "[watchdog] procd did not bring up new version yet, launching directly" >> %s
    TMPDIR=%s GOMEMLIMIT=120MiB GOGC=50 %s -config %s >> %s 2>&1 &
fi
OK=0
for i in 1 2 3 4 5 6 7 8 9 10; do
    sleep 1
    if wget -qO- %s 2>/dev/null | grep -q %s || curl -fsS %s 2>/dev/null | grep -q %s; then
        OK=1
        echo "[watchdog] verified new version %s is live" >> %s
        commit_runtime
        break
    fi
done
if [ "$OK" = "0" ] && [ -f %s.bak ]; then
    echo "[watchdog] new version failed health check, rolling back binary and runtime" >> %s
    killall -9 reader 2>/dev/null || true
    rollback_runtime
    cp -f %s.bak %s
    chmod +x %s
    if [ -x /etc/init.d/reader ]; then
        /etc/init.d/reader restart >> %s 2>&1 || /etc/init.d/reader start >> %s 2>&1
    fi
    sleep 1
    TMPDIR=%s GOMEMLIMIT=120MiB GOGC=50 %s -config %s >> %s 2>&1 &
fi
`,
		q(runtimeRoot), oldPID, exePath, expectedVersion, q(logFile),
		oldPID, oldPID, q(logFile), q(logFile),
		q(healthCheckURL), q(expectedVersion), q(healthCheckURL), q(expectedVersion), q(logFile),
		q(tmpDir), q(exePath), q(cfgPath), q(logFile),
		q(healthCheckURL), q(expectedVersion), q(healthCheckURL), q(expectedVersion), expectedVersion, q(logFile),
		q(exePath), q(logFile), q(exePath), q(exePath), q(exePath),
		q(logFile), q(logFile), q(tmpDir), q(exePath), q(cfgPath), q(logFile),
	)
	return script
}

func healthHostPort(listen string) (string, string) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		if idx := strings.LastIndex(listen, ":"); idx >= 0 {
			port = listen[idx+1:]
		}
		host = ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8380"
	}
	return host, port
}

func healthURL(host, port string) string {
	return "http://" + net.JoinHostPort(host, port) + "/api/v1/health"
}

// HealthCheckURL returns a local health URL that also works when the server
// listens on a specific LAN address rather than 0.0.0.0.
func HealthCheckURL(listen string) string {
	host, port := healthHostPort(listen)
	return healthURL(host, port)
}

// MarkRuntimeRollbackWatchdogProcess must be called in the watchdog CLI mode
// before starting goroutines. The old updater uses killall -9 reader; keeping
// this thread group leader named reader-rollback lets the watchdog survive it.
func MarkRuntimeRollbackWatchdogProcess() error {
	runtime.LockOSThread()
	name := []byte("reader-rollback\x00")
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, syscall.PR_SET_NAME, uintptr(unsafe.Pointer(&name[0])), 0, 0, 0, 0)
	runtime.KeepAlive(name)
	if errno != 0 {
		runtime.UnlockOSThread()
		return errno
	}
	return nil
}

// SpawnRuntimeActivationWatchdog starts a separate copy of the new binary so
// its rollback remains available if the v0.3.10 updater restores the executable
// and the main service exits before health succeeds.
func SpawnRuntimeActivationWatchdog(cfg *config.Config, grace time.Duration) error {
	if cfg == nil {
		return fmt.Errorf("missing config for runtime rollback watchdog")
	}
	if !RuntimeActivationPending(cfg.DataDir) {
		return nil
	}
	if grace <= 0 || grace > maxRuntimeRollbackGracePeriod {
		return fmt.Errorf("invalid runtime watchdog grace period %s", grace)
	}
	failAndRollback := func(cause error) error {
		if rollbackErr := rollbackRuntimeActivation(cfg.DataDir); rollbackErr != nil {
			return fmt.Errorf("%v; runtime pointer rollback failed: %w", cause, rollbackErr)
		}
		return cause
	}
	if err := validateUpdateStorage(cfg.DataDir, cfg.TmpDir()); err != nil {
		return failAndRollback(fmt.Errorf("runtime watchdog storage validation failed: %w", err))
	}
	exePath, err := os.Executable()
	if err != nil {
		return failAndRollback(fmt.Errorf("locate runtime watchdog executable: %w", err))
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	cfgPath := cfg.ConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(cfg.DataDir, "config.json")
	}
	logPath := filepath.Join(cfg.TmpDir(), "runtime_rollback_watchdog.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return failAndRollback(fmt.Errorf("open external-disk runtime watchdog log: %w", err))
	}
	cmd := exec.Command(exePath, "-runtime-rollback-watchdog="+grace.String(), "-config", cfgPath)
	cmd.Env = setEnvValue(os.Environ(), "TMPDIR", cfg.TmpDir())
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return failAndRollback(fmt.Errorf("start detached runtime rollback watchdog: %w", err))
	}
	_ = logFile.Close()
	if err := cmd.Process.Release(); err != nil {
		return failAndRollback(fmt.Errorf("release detached runtime rollback watchdog: %w", err))
	}
	return nil
}

// RunRuntimeActivationWatchdog is the child-process mode used during first
// bootstrap. It commits only a matching healthy version; otherwise it restores
// the pointer state recorded before installation (including no prior runtime).
func RunRuntimeActivationWatchdog(dataDir, listen, expectedVersion string, grace time.Duration) error {
	if grace <= 0 || grace > maxRuntimeRollbackGracePeriod {
		return fmt.Errorf("invalid runtime watchdog grace period %s", grace)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !RuntimeActivationPending(dataDir) {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining > 500*time.Millisecond {
			remaining = 500 * time.Millisecond
		}
		time.Sleep(remaining)
	}
	if !RuntimeActivationPending(dataDir) {
		return nil
	}
	if healthReportsVersion(listen, expectedVersion) {
		return CommitRuntimeActivation(dataDir)
	}
	return rollbackRuntimeActivation(dataDir)
}

func healthReportsVersion(listen, expectedVersion string) bool {
	resp, err := localHealthClient.Get(HealthCheckURL(listen))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&health); err != nil {
		return false
	}
	return health.Status == "ok" && health.Version == expectedVersion
}

// HealthCheckHasVersion is used by the main process before committing a
// runtime activation; an old v0.3.10 service answering HTTP 200 is not success.
func HealthCheckHasVersion(listen, expectedVersion string) bool {
	return healthReportsVersion(listen, expectedVersion)
}

func setEnvValue(env []string, key, value string) []string {
	prefix := key + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// LoopbackHealthAddress keeps the v0.3.10 watchdog compatible with LAN-only
// binds: its legacy probe is hard-coded to 127.0.0.1. The returned listener is
// loopback-only and is needed only when the main listener uses a specific IP.
func LoopbackHealthAddress(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", false
	}
	if port == "" {
		return "", false
	}
	return net.JoinHostPort("127.0.0.1", port), true
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (u *Updater) downloadFile(ctx context.Context, url, destPath string, maxBytes int64) (string, int64, error) {
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
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBytes+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return "", n, copyErr
	}
	if syncErr != nil {
		return "", n, syncErr
	}
	if closeErr != nil {
		return "", n, closeErr
	}
	if n > maxBytes {
		return "", n, fmt.Errorf("download exceeds size limit (%d bytes)", maxBytes)
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
	// Rename the running executable to .bak; Linux keeps the old process inode open.
	if err := os.Rename(exePath, bakPath); err != nil {
		return err
	}
	if err := os.Rename(newBinPath, exePath); err == nil {
		return nil
	}
	// Cross-device fallback stages beside the executable; production checks above
	// require that both the executable and updater staging directory are external.
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
