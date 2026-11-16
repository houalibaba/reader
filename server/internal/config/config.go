package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const MountMarkerFile = ".reader-disk"

type BackupConfig struct {
	Time       string `json:"time"`
	KeepDaily  int    `json:"keep_daily"`
	KeepWeekly int    `json:"keep_weekly"`
}

type LLMConfig struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
}

type UpdateConfig struct {
	Repo      string `json:"repo"`
	MirrorURL string `json:"mirror_url"`
}

type Config struct {
	ConfigPath            string       `json:"-"`
	Listen                string       `json:"listen"`
	DataDir               string       `json:"data_dir"`
	SessionTimeoutMinutes int          `json:"session_timeout_minutes"`
	GuardianPin           string       `json:"guardian_pin"`
	Backup                BackupConfig `json:"backup"`
	LLM                   LLMConfig    `json:"llm"`
	Update                UpdateConfig `json:"update"`
	UploadMaxMB           int64        `json:"upload_max_mb"`
	MinFreeDiskMB         uint64       `json:"min_free_disk_mb"`
}

func DefaultConfig() *Config {
	return &Config{
		Listen:                "192.168.100.1:8380",
		DataDir:               "/mnt/sda1/reader",
		SessionTimeoutMinutes: 30,
		GuardianPin:           "1234",
		Backup: BackupConfig{
			Time:       "03:30",
			KeepDaily:  7,
			KeepWeekly: 4,
		},
		LLM: LLMConfig{
			APIURL: "https://openrouter.ai/api/v1/chat/completions",
			APIKey: "",
			Model:  "nvidia/nemotron-3-ultra-550b-a55b:free",
		},
		Update: UpdateConfig{
			Repo:      "houalibaba/Reader",
			MirrorURL: "",
		},
		UploadMaxMB:   200,
		MinFreeDiskMB: 1024,
	}
}

func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.SessionTimeoutMinutes <= 0 {
		cfg.SessionTimeoutMinutes = 30
	}
	if cfg.UploadMaxMB <= 0 {
		cfg.UploadMaxMB = 200
	}
	if cfg.MinFreeDiskMB == 0 {
		cfg.MinFreeDiskMB = 1024
	}
	if cfg.LLM.APIURL == "" {
		cfg.LLM.APIURL = "https://openrouter.ai/api/v1/chat/completions"
	}
	if cfg.LLM.Model == "" {
		cfg.LLM.Model = "nvidia/nemotron-3-ultra-550b-a55b:free"
	}
	if cfg.Update.Repo == "" {
		cfg.Update.Repo = "houalibaba/Reader"
	}
	return cfg, nil
}

// VerifyDiskMount checks that the external drive mount marker exists and ensures subdirectories exist.
// Prevents writing to router flash memory when external storage is unmounted.
func (c *Config) VerifyDiskMount() error {
	dataInfo, err := os.Lstat(c.DataDir)
	if err != nil {
		return fmt.Errorf("stat data directory %s: %w", c.DataDir, err)
	}
	if !dataInfo.IsDir() || dataInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("data directory %s must be a real directory, not a symlink", c.DataDir)
	}
	marker := filepath.Join(c.DataDir, MountMarkerFile)
	info, err := os.Lstat(marker)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("external disk mount marker missing (%s): refusing to start on router flash", marker)
		}
		return fmt.Errorf("stat mount marker %s: %w", marker, err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("mount marker .reader-disk must be a regular file, not a directory or symlink")
	}
	if err := c.verifyExternalDataVolume(); err != nil {
		return err
	}
	tmpInfo, err := os.Lstat(c.TmpDir())
	if err == nil {
		if !tmpInfo.IsDir() || tmpInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("temporary directory %s must be a real directory, not a symlink", c.TmpDir())
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat temporary directory %s: %w", c.TmpDir(), err)
	}

	dirs := []string{
		c.DBDir(),
		c.MediaDir(),
		filepath.Join(c.MediaDir(), "stories"),
		filepath.Join(c.MediaDir(), "audio"),
		c.TmpDir(),
		c.BackupDir(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

func (c *Config) verifyExternalDataVolume() error {
	dataDir := filepath.Clean(c.DataDir)
	if !strings.HasPrefix(dataDir, "/mnt/") {
		return nil
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		return fmt.Errorf("stat external data directory %s: %w", dataDir, err)
	}
	rootInfo, err := os.Stat("/")
	if err != nil {
		return fmt.Errorf("stat root filesystem: %w", err)
	}
	dataStat, dataOK := dataInfo.Sys().(*syscall.Stat_t)
	rootStat, rootOK := rootInfo.Sys().(*syscall.Stat_t)
	if !dataOK || !rootOK {
		return errors.New("cannot verify external data filesystem")
	}
	if dataStat.Dev == rootStat.Dev {
		return fmt.Errorf("%s is not on a mounted external filesystem: refusing to start on router flash", dataDir)
	}
	return nil
}

func (c *Config) DBDir() string     { return filepath.Join(c.DataDir, "data") }
func (c *Config) DBPath() string    { return filepath.Join(c.DBDir(), "reader.db") }
func (c *Config) MediaDir() string  { return filepath.Join(c.DataDir, "media") }
func (c *Config) TmpDir() string    { return filepath.Join(c.DataDir, "tmp") }
func (c *Config) BackupDir() string { return filepath.Join(c.DataDir, "backups") }

// CheckFreeDiskMB returns available disk space in MB and whether it meets MinFreeDiskMB.
func (c *Config) CheckFreeDiskMB() (uint64, bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(c.DataDir, &stat); err != nil {
		return 0, false, fmt.Errorf("statfs %s: %w", c.DataDir, err)
	}
	freeMB := (stat.Bavail * uint64(stat.Bsize)) / (1024 * 1024)
	return freeMB, freeMB >= c.MinFreeDiskMB, nil
}
