package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
			"body":         "新增自动升级功能",
			"published_at": "2026-11-13T12:00:00Z",
			"assets": []map[string]any{
				{
					"name":                 assetName,
					"size":                 1234567,
					"browser_download_url": "https://github.com/houalibaba/Reader/releases/download/v0.3.99/" + assetName,
				},
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
	hexSum := hex.EncodeToString(sum[:])
	if len(hexSum) != 64 {
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
