package updater

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
)

const maxGitHubFallbackBodyBytes = 4 << 20

var (
	releaseAnchorRE = regexp.MustCompile(`(?is)<a\b[^>]*>`)
	hrefAttributeRE = regexp.MustCompile(`(?is)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

type atomReleaseFeed struct {
	Entries []atomReleaseEntry `xml:"entry"`
}

type atomReleaseEntry struct {
	Title     string     `xml:"title"`
	Updated   string     `xml:"updated"`
	Published string     `xml:"published"`
	Links     []atomLink `xml:"link"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

// isGitHubRateLimitResponse deliberately does not treat every 403 as a quota
// error. GitHub documents 429, x-ratelimit-remaining: 0, and explicit secondary
// rate-limit messages as the distinguishing signals.
func isGitHubRateLimitResponse(status int, headers http.Header, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	if strings.TrimSpace(headers.Get("X-RateLimit-Remaining")) == "0" {
		return true
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "rate limit") || strings.Contains(message, "rate-limit") || strings.Contains(message, "abuse detection")
}

func (u *Updater) checkUpdateFromReleaseFeed(ctx context.Context, repo, mirrorOverride string) (*CheckResult, error) {
	repoParts, err := safeGitHubRepoParts(repo)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimRight(u.githubBaseURL, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil {
		return nil, fmt.Errorf("GitHub 公共站点基址无效")
	}

	feedURL := *base
	feedURL.Path = "/" + strings.Join(repoParts, "/") + "/releases.atom"
	feedResp, err := u.getFallbackResponse(ctx, feedURL.String(), "application/atom+xml, application/xml;q=0.9, */*;q=0.1")
	if err != nil {
		return nil, fmt.Errorf("读取 Releases Atom 源: %w", err)
	}
	feedBody, readErr := readFallbackBody(feedResp.Body)
	_ = feedResp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("读取 Releases Atom 源内容: %w", readErr)
	}

	var feed atomReleaseFeed
	if err := xml.Unmarshal(feedBody, &feed); err != nil {
		return nil, fmt.Errorf("解析 Releases Atom 源失败: %w", err)
	}
	var selected *atomReleaseEntry
	var selectedTag, selectedURL string
	for i := range feed.Entries {
		entry := &feed.Entries[i]
		for _, link := range entry.Links {
			if !strings.EqualFold(link.Rel, "alternate") {
				continue
			}
			tag, releaseURL, ok := parseStableTagReleaseURL(link.Href, &feedURL, repoParts)
			if ok {
				selected, selectedTag, selectedURL = entry, tag, releaseURL
				break
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("Releases Atom 源中没有可识别的稳定版发布")
	}

	assetsURL := *base
	assetsURL.Path = "/" + strings.Join(repoParts, "/") + "/releases/expanded_assets/" + selectedTag
	assetsResp, err := u.getFallbackResponse(ctx, assetsURL.String(), "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")
	if err != nil {
		return nil, fmt.Errorf("读取 Release 资产清单: %w", err)
	}
	assetsBody, readErr := readFallbackBody(assetsResp.Body)
	_ = assetsResp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("读取 Release 资产清单内容: %w", readErr)
	}
	assets, err := parseReleaseAssetLinks(string(assetsBody), &assetsURL, repoParts, selectedTag)
	if err != nil {
		return nil, err
	}

	assetName := ExpectedAssetName()
	assetURL, assetOK := assets[assetName]
	runtimeURL, runtimeOK := assets[runtimeAssetName]
	checksumURL, checksumOK := assets["checksums.txt"]
	if !assetOK || !runtimeOK || !checksumOK {
		return nil, fmt.Errorf("Release 资产清单缺少必要文件（%s、%s 或 checksums.txt）；拒绝猜测下载地址", assetName, runtimeAssetName)
	}

	mirror := u.effectiveMirror(mirrorOverride)
	assetURL = applyMirror(assetURL, mirror)
	runtimeURL = applyMirror(runtimeURL, mirror)
	checksumURL = applyMirror(checksumURL, mirror)
	assetSize, err := u.headAssetSize(ctx, assetURL, base.Scheme)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的精确资产大小失败: %w", assetName, err)
	}
	runtimeSize, err := u.headAssetSize(ctx, runtimeURL, base.Scheme)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的精确资产大小失败: %w", runtimeAssetName, err)
	}
	if assetSize > 512<<20 || runtimeSize > runtimeBundleMaxBytes {
		return nil, fmt.Errorf("Release 资产超过更新器的安全大小上限")
	}

	publishedAt := selected.Published
	if publishedAt == "" {
		publishedAt = selected.Updated
	}
	return &CheckResult{
		CurrentVersion:   CurrentVersion,
		LatestVersion:    selectedTag,
		HasUpdate:        CompareVersions(CurrentVersion, selectedTag),
		Arch:             runtime.GOOS + "/" + runtime.GOARCH,
		Repo:             repo,
		ReleaseURL:       selectedURL,
		AssetName:        assetName,
		AssetURL:         assetURL,
		AssetSize:        assetSize,
		RuntimeAssetName: runtimeAssetName,
		RuntimeAssetURL:  runtimeURL,
		RuntimeAssetSize: runtimeSize,
		ChecksumURL:      checksumURL,
		PublishedAt:      publishedAt,
		ReleaseNotes:     "通过 GitHub 公共 Releases 源读取版本（REST API 限流）；请打开发布页查看完整说明。",
	}, nil
}

func (u *Updater) getFallbackResponse(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "Reader-AutoUpdater/1.0")
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func readFallbackBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxGitHubFallbackBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxGitHubFallbackBodyBytes {
		return nil, fmt.Errorf("响应超过 %d 字节上限", maxGitHubFallbackBodyBytes)
	}
	return data, nil
}

func safeGitHubRepoParts(repo string) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("仓库名必须为 owner/repo 格式，拒绝读取公共 Release")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("仓库名无效，拒绝读取公共 Release")
		}
		for _, r := range part {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
				return nil, fmt.Errorf("仓库名含有不安全字符，拒绝读取公共 Release")
			}
		}
	}
	return parts, nil
}

func parseStableTagReleaseURL(raw string, feedURL *url.URL, repoParts []string) (tag, releaseURL string, ok bool) {
	parsed, err := feedURL.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.EqualFold(parsed.Scheme, feedURL.Scheme) || !strings.EqualFold(parsed.Host, feedURL.Host) {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 5 || !strings.EqualFold(parts[0], repoParts[0]) || !strings.EqualFold(parts[1], repoParts[1]) || parts[2] != "releases" || parts[3] != "tag" {
		return "", "", false
	}
	tag = parts[4]
	if !isStableReleaseTag(tag) {
		return "", "", false
	}
	return tag, parsed.String(), true
}

func isStableReleaseTag(tag string) bool {
	if strings.ContainsAny(tag, "-+") {
		return false
	}
	_, ok := parseSemver(tag)
	return ok
}

func parseReleaseAssetLinks(body string, pageURL *url.URL, repoParts []string, tag string) (map[string]string, error) {
	assets := make(map[string]string)
	for _, anchor := range releaseAnchorRE.FindAllString(body, -1) {
		match := hrefAttributeRE.FindStringSubmatch(anchor)
		if len(match) == 0 {
			continue
		}
		href := match[1]
		if href == "" {
			href = match[2]
		}
		if href == "" {
			href = match[3]
		}
		assetURL, err := pageURL.Parse(html.UnescapeString(strings.TrimSpace(href)))
		if err != nil || assetURL.User != nil || assetURL.RawQuery != "" || assetURL.Fragment != "" || !strings.EqualFold(assetURL.Scheme, pageURL.Scheme) || !strings.EqualFold(assetURL.Host, pageURL.Host) {
			continue
		}
		parts := strings.Split(strings.Trim(assetURL.Path, "/"), "/")
		if len(parts) != 6 || !strings.EqualFold(parts[0], repoParts[0]) || !strings.EqualFold(parts[1], repoParts[1]) || parts[2] != "releases" || parts[3] != "download" || parts[4] != tag {
			continue
		}
		name := parts[5]
		if name == "" || name == "." || name == ".." {
			continue
		}
		resolved := assetURL.String()
		if existing, found := assets[name]; found && existing != resolved {
			return nil, fmt.Errorf("Release 资产清单中 %s 有重复下载链接，拒绝继续", name)
		}
		assets[name] = resolved
	}
	return assets, nil
}

func (u *Updater) headAssetSize(ctx context.Context, rawURL, baseScheme string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Reader-AutoUpdater/1.0")
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if baseScheme == "https" && (resp.Request == nil || resp.Request.URL.Scheme != "https") {
		return 0, fmt.Errorf("下载地址重定向降级到非 HTTPS")
	}
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("服务器未提供有效 Content-Length")
	}
	return resp.ContentLength, nil
}
