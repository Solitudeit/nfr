package server

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/store"
	"github.com/solitudeit/nfr/internal/version"
)

const (
	defaultRepoURL            = "https://github.com/Solitudeit/nfr"
	defaultManifestURL        = "https://raw.githubusercontent.com/Solitudeit/nfr/version-manifest/version.json"
	defaultReleasesAPIURL     = "https://api.github.com/repos/Solitudeit/nfr/releases?per_page=10"
	updateCheckInterval       = 6 * time.Hour
	maxArchiveDownloadBytes   = 128 << 20
	maxMetadataDownloadBytes  = 2 << 20
	mirrorProbeTimeout        = 3500 * time.Millisecond
)

// DefaultDownloadMirrors lists verified GitHub Release / Raw acceleration mirrors.
var DefaultDownloadMirrors = []api.MirrorNode{
	{ID: "edgeone", Name: "EdgeOne 镜像", Prefix: "https://edgeone.gh-proxy.com/", ProbeURL: "https://edgeone.gh-proxy.com/"},
	{ID: "gh-proxy-org", Name: "gh-proxy.org", Prefix: "https://gh-proxy.org/", ProbeURL: "https://gh-proxy.org/"},
	{ID: "gh-proxy-com", Name: "gh-proxy.com", Prefix: "https://gh-proxy.com/", ProbeURL: "https://gh-proxy.com/"},
	{ID: "ghfast", Name: "ghfast.top", Prefix: "https://ghfast.top/", ProbeURL: "https://ghfast.top/"},
	{ID: "yylx", Name: "git.yylx.win", Prefix: "https://git.yylx.win/", ProbeURL: "https://git.yylx.win/"},
}

// EffectiveMirrors returns the configured acceleration mirror list, allowing
// override via NFR_MIRROR_PREFIXES (comma-separated URL prefixes).
func EffectiveMirrors() []api.MirrorNode {
	raw := strings.TrimSpace(os.Getenv("NFR_MIRROR_PREFIXES"))
	if raw == "" {
		out := make([]api.MirrorNode, len(DefaultDownloadMirrors))
		copy(out, DefaultDownloadMirrors)
		return out
	}
	var out []api.MirrorNode
	for i, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		label := strings.TrimPrefix(strings.TrimPrefix(p, "https://"), "http://")
		label = strings.TrimRight(label, "/")
		out = append(out, api.MirrorNode{
			ID:       fmt.Sprintf("mirror-%d", i+1),
			Name:     label,
			Prefix:   p,
			ProbeURL: p,
		})
	}
	if len(out) == 0 {
		out = make([]api.MirrorNode, len(DefaultDownloadMirrors))
		copy(out, DefaultDownloadMirrors)
	}
	return out
}

type manifestEntry struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
	Notes       string `json:"notes,omitempty"`
	URL         string `json:"url,omitempty"`
}

type versionManifest struct {
	Stable  *manifestEntry `json:"stable,omitempty"`
	Preview *manifestEntry `json:"preview,omitempty"`
}

type githubRelease struct {
	TagName     string `json:"tag_name"`
	Prerelease  bool   `json:"prerelease"`
	Draft       bool   `json:"draft"`
	PublishedAt string `json:"published_at"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
}

type updater struct {
	srv *Server

	mu            sync.Mutex
	latest        string
	isPrerelease  bool
	publishedAt   string
	notes         string
	releaseURL    string
	checkedAt     *time.Time
	lastError     string
	sourceUsed    string
	mirrorUsed    string
	upgrade       api.UpgradeProgress
	lastProbe     *api.UpdateMirrorProbe
	lastProbeTime time.Time
	// restartHook allows tests to intercept the final restart step safely.
	restartHook func(exePath string)
}

func newUpdater(s *Server) *updater {
	return &updater{
		srv:     s,
		upgrade: api.UpgradeProgress{State: "idle", Percent: 0},
	}
}

func currentAppVersion() string {
	if v := strings.TrimSpace(os.Getenv("NFR_UPDATE_CURRENT_VERSION")); v != "" {
		return v
	}
	return version.Version
}

func normalizeChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case "preview":
		return "preview"
	case "stable":
		return "stable"
	default:
		return "auto"
	}
}

func (u *updater) startBackground(ctx context.Context) {
	cleanupStaleUpgradeDirs()
	if os.Getenv("NFR_DISABLE_UPDATE_CHECK") == "1" {
		return
	}
	// Do not make unsolicited external requests for local dev builds unless a
	// manifest URL override was explicitly configured (e.g. integration tests).
	if version.IsDev(currentAppVersion()) && strings.TrimSpace(os.Getenv("NFR_UPDATE_MANIFEST_URL")) == "" {
		return
	}

	u.srv.wg.Add(1)
	go func() {
		defer u.srv.wg.Done()
		initialDelay := 12 * time.Second
		if strings.TrimSpace(os.Getenv("NFR_UPDATE_MANIFEST_URL")) != "" {
			initialDelay = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(initialDelay):
		}
		u.maybeBackgroundCheck(ctx)

		ticker := time.NewTicker(updateCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				u.maybeBackgroundCheck(ctx)
			}
		}
	}()
}

func (u *updater) maybeBackgroundCheck(ctx context.Context) {
	set := u.srv.Settings()
	if set.DisableAutoUpdateCheck {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _ = u.check(checkCtx, "", normalizeChannel(set.UpdateChannel))
}

func (u *updater) canSelfUpdate() (bool, string, string) {
	if os.Getenv("NFR_DISABLE_SELF_UPDATE") == "1" {
		return false, "服务端已通过 NFR_DISABLE_SELF_UPDATE 禁用一键自升级", ""
	}
	if os.Getenv("NFR_ALLOW_SELF_UPDATE_IN_CONTAINER") != "1" {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			return false, "当前运行在 Docker 容器中，请通过拉取新镜像更新", ""
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return false, "无法定位当前程序路径: " + err.Error(), ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	probe, err := os.CreateTemp(dir, ".nfr-upgrade-write-test-*")
	if err != nil {
		return false, fmt.Sprintf("程序目录 %s 不可写，请以具备写权限的用户或手动替换升级", dir), exe
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true, "", exe
}

func (u *updater) status() api.UpdateStatus {
	set := u.srv.Settings()
	cur := currentAppVersion()
	canUp, reason, _ := u.canSelfUpdate()

	u.mu.Lock()
	defer u.mu.Unlock()
	hasUp := version.IsNewer(u.latest, cur)
	var checkedCopy *time.Time
	if u.checkedAt != nil {
		t := *u.checkedAt
		checkedCopy = &t
	}
	return api.UpdateStatus{
		CurrentVersion:     cur,
		LatestVersion:      u.latest,
		HasUpdate:          hasUp,
		IsPrerelease:       u.isPrerelease,
		PublishedAt:        u.publishedAt,
		Notes:              u.notes,
		ReleaseURL:         u.releaseURL,
		CheckedAt:          checkedCopy,
		LastError:          u.lastError,
		AutoCheck:          !set.DisableAutoUpdateCheck,
		Channel:            normalizeChannel(set.UpdateChannel),
		CanSelfUpdate:      canUp,
		CannotUpdateReason: reason,
		SourceUsed:         u.sourceUsed,
		MirrorUsed:         u.mirrorUsed,
		Mirrors:            EffectiveMirrors(),
		Upgrade:            u.upgrade,
	}
}

func (u *updater) latestClientVersion() string {
	cur := currentAppVersion()
	u.mu.Lock()
	latest := u.latest
	u.mu.Unlock()
	if latest != "" && (version.IsDev(cur) || version.Compare(latest, cur) >= 0) {
		return latest
	}
	if !version.IsDev(cur) {
		return cur
	}
	return ""
}

func (u *updater) progress() api.UpgradeProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrade
}

func (u *updater) setProgress(p api.UpgradeProgress) {
	u.mu.Lock()
	u.upgrade = p
	u.mu.Unlock()
}

// ProbeMirrors races all configured acceleration mirrors from the server host
// and returns the lowest-latency reachable mirror.
func (u *updater) probeMirrors(ctx context.Context, force bool) api.UpdateMirrorProbe {
	u.mu.Lock()
	if !force && u.lastProbe != nil && time.Since(u.lastProbeTime) < 60*time.Second {
		cp := *u.lastProbe
		u.mu.Unlock()
		return cp
	}
	u.mu.Unlock()

	mirrors := EffectiveMirrors()
	if len(mirrors) == 0 {
		return api.UpdateMirrorProbe{OK: false, Message: "未配置镜像节点"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, mirrorProbeTimeout)
	defer cancel()

	type hit struct {
		node  api.MirrorNode
		rttMs int64
	}
	ch := make(chan hit, len(mirrors))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: mirrorProbeTimeout}

	for _, m := range mirrors {
		wg.Add(1)
		go func(node api.MirrorNode) {
			defer wg.Done()
			target := node.ProbeURL
			sep := "?"
			if strings.Contains(target, "?") {
				sep = "&"
			}
			req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, fmt.Sprintf("%s%s_nfr_ping=%d", target, sep, time.Now().UnixNano()), nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "nfr-updater/"+currentAppVersion())
			t0 := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			_, _ = io.CopyN(io.Discard, resp.Body, 4096)
			_ = resp.Body.Close()
			if resp.StatusCode >= 500 {
				return
			}
			rtt := time.Since(t0).Milliseconds()
			if rtt < 1 {
				rtt = 1
			}
			select {
			case ch <- hit{node: node, rttMs: rtt}:
			default:
			}
		}(m)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	var res api.UpdateMirrorProbe
	if first, ok := <-ch; ok {
		cancel()
		res = api.UpdateMirrorProbe{
			OK:     true,
			Mirror: first.node,
			RTTMs:  first.rttMs,
		}
	} else {
		res = api.UpdateMirrorProbe{
			OK:      false,
			Mirror:  mirrors[0],
			Message: "镜像测速超时，将使用默认镜像",
		}
	}

	u.mu.Lock()
	u.lastProbe = &res
	u.lastProbeTime = time.Now()
	u.mu.Unlock()
	return res
}

func (u *updater) check(ctx context.Context, source, channel string) (api.UpdateStatus, error) {
	ch := normalizeChannel(channel)
	if ch == "auto" {
		cur := currentAppVersion()
		if version.IsPrerelease(cur) || version.IsDev(cur) {
			ch = "preview"
		} else {
			ch = "stable"
		}
	}

	manifestURL := strings.TrimSpace(os.Getenv("NFR_UPDATE_MANIFEST_URL"))
	if manifestURL == "" {
		manifestURL = defaultManifestURL
	}
	apiURL := strings.TrimSpace(os.Getenv("NFR_UPDATE_API_URL"))
	if apiURL == "" {
		apiURL = defaultReleasesAPIURL
	}

	var (
		entry      *manifestEntry
		usedSource string
		usedMirror string
		lastErr    error
	)

	tryManifest := func(targetURL, srcLabel, mirLabel string) bool {
		m, err := fetchManifest(ctx, targetURL)
		if err != nil {
			lastErr = err
			return false
		}
		picked := pickManifestEntry(m, ch)
		if picked == nil || strings.TrimSpace(picked.Version) == "" {
			lastErr = errors.New("版本清单中暂无匹配的发行版本")
			return false
		}
		entry = picked
		usedSource = srcLabel
		usedMirror = mirLabel
		return true
	}

	tryReleasesAPI := func() bool {
		e, err := fetchLatestFromGitHubAPI(ctx, apiURL, ch)
		if err != nil {
			lastErr = err
			return false
		}
		entry = e
		usedSource = "github"
		usedMirror = ""
		return true
	}

	switch strings.ToLower(strings.TrimSpace(source)) {
	case "mirror":
		probe := u.probeMirrors(ctx, false)
		mirrors := orderedMirrors(probe.Mirror.Prefix)
		ok := false
		for _, m := range mirrors {
			if tryManifest(applyMirrorPrefix(m.Prefix, manifestURL), "mirror", m.Name) {
				ok = true
				break
			}
		}
		if !ok {
			if !tryManifest(manifestURL, "github", "") {
				tryReleasesAPI()
			}
		}
	case "github":
		if !tryManifest(manifestURL, "github", "") {
			tryReleasesAPI()
		}
	default:
		// Auto mode: direct manifest first, then fastest mirror, then GitHub Releases API.
		if !tryManifest(manifestURL, "github", "") {
			probe := u.probeMirrors(ctx, false)
			ok := false
			for _, m := range orderedMirrors(probe.Mirror.Prefix) {
				if tryManifest(applyMirrorPrefix(m.Prefix, manifestURL), "mirror", m.Name) {
					ok = true
					break
				}
			}
			if !ok {
				tryReleasesAPI()
			}
		}
	}

	now := time.Now().UTC()
	u.mu.Lock()
	u.checkedAt = &now
	if entry != nil {
		u.latest = strings.TrimSpace(entry.Version)
		u.isPrerelease = version.IsPrerelease(u.latest)
		u.publishedAt = strings.TrimSpace(entry.PublishedAt)
		u.notes = strings.TrimSpace(entry.Notes)
		u.releaseURL = strings.TrimSpace(entry.URL)
		if u.releaseURL == "" && u.latest != "" {
			u.releaseURL = defaultRepoURL + "/releases/tag/" + u.latest
		}
		u.lastError = ""
		u.sourceUsed = usedSource
		u.mirrorUsed = usedMirror
	} else if lastErr != nil {
		u.lastError = lastErr.Error()
	}
	u.mu.Unlock()

	st := u.status()
	if entry == nil && lastErr != nil {
		return st, lastErr
	}
	return st, nil
}

func cleanupStaleUpgradeDirs() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".nfr-upgrade-") {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func withCacheBuster(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("_t", strconv.FormatInt(time.Now().Unix(), 10))
	u.RawQuery = q.Encode()
	return u.String()
}

func fetchManifest(ctx context.Context, targetURL string) (*versionManifest, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, withCacheBuster(targetURL), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nfr-updater/"+currentAppVersion())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("版本清单返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataDownloadBytes))
	if err != nil {
		return nil, err
	}
	var m versionManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析版本清单失败: %w", err)
	}
	return &m, nil
}

func pickManifestEntry(m *versionManifest, channel string) *manifestEntry {
	if m == nil {
		return nil
	}
	if channel == "stable" {
		if m.Stable != nil && strings.TrimSpace(m.Stable.Version) != "" {
			return m.Stable
		}
		return nil
	}
	// preview channel picks whichever of stable or preview is higher SemVer.
	hasStable := m.Stable != nil && strings.TrimSpace(m.Stable.Version) != ""
	hasPreview := m.Preview != nil && strings.TrimSpace(m.Preview.Version) != ""
	switch {
	case hasStable && hasPreview:
		if version.Compare(m.Stable.Version, m.Preview.Version) >= 0 {
			return m.Stable
		}
		return m.Preview
	case hasPreview:
		return m.Preview
	case hasStable:
		return m.Stable
	default:
		return nil
	}
}

func fetchLatestFromGitHubAPI(ctx context.Context, apiURL, channel string) (*manifestEntry, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nfr-updater/"+currentAppVersion())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataDownloadBytes))
	if err != nil {
		return nil, err
	}
	var rels []githubRelease
	if err := json.Unmarshal(raw, &rels); err != nil {
		return nil, err
	}
	var best *githubRelease
	for i := range rels {
		r := &rels[i]
		if r.Draft || strings.TrimSpace(r.TagName) == "" {
			continue
		}
		if channel == "stable" && (r.Prerelease || version.IsPrerelease(r.TagName)) {
			continue
		}
		if best == nil || version.Compare(r.TagName, best.TagName) > 0 {
			best = r
		}
	}
	if best == nil {
		return nil, errors.New("未找到可用的发行版本")
	}
	return &manifestEntry{
		Version:     best.TagName,
		PublishedAt: best.PublishedAt,
		Notes:       strings.TrimSpace(best.Body),
		URL:         best.HTMLURL,
	}, nil
}

func orderedMirrors(preferredPrefix string) []api.MirrorNode {
	all := EffectiveMirrors()
	if preferredPrefix == "" {
		return all
	}
	pref := ensureTrailingSlash(preferredPrefix)
	out := make([]api.MirrorNode, 0, len(all))
	for _, m := range all {
		if ensureTrailingSlash(m.Prefix) == pref {
			out = append(out, m)
		}
	}
	for _, m := range all {
		if ensureTrailingSlash(m.Prefix) != pref {
			out = append(out, m)
		}
	}
	return out
}

func validateAllowedMirror(prefix string) (api.MirrorNode, bool) {
	want := ensureTrailingSlash(strings.TrimSpace(prefix))
	if want == "" {
		return api.MirrorNode{}, false
	}
	for _, m := range EffectiveMirrors() {
		if ensureTrailingSlash(m.Prefix) == want {
			return m, true
		}
	}
	return api.MirrorNode{}, false
}

func ensureTrailingSlash(s string) string {
	if s == "" || strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
}

func applyMirrorPrefix(prefix, rawURL string) string {
	// Never double-prefix local HTTP test URLs.
	if strings.HasPrefix(rawURL, "http://127.0.0.1") || strings.HasPrefix(rawURL, "http://localhost") {
		return rawURL
	}
	return ensureTrailingSlash(prefix) + rawURL
}

func releaseArchiveName(ver, goos, goarch string) (string, error) {
	arch := goarch
	if goos == "linux" && goarch == "arm" {
		arch = "armv7"
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	osName := goos
	if goos == "darwin" {
		osName = "macos"
	}
	if ver == "" {
		return "", errors.New("目标版本号为空")
	}
	return fmt.Sprintf("nfr_%s_%s_%s%s", ver, osName, arch, ext), nil
}

func (u *updater) startUpgrade(source, mirrorPrefix, targetVersion string) (api.UpgradeProgress, error) {
	canUp, reason, exePath := u.canSelfUpdate()
	if !canUp {
		return u.progress(), errBad("self_update", reason)
	}

	src := strings.ToLower(strings.TrimSpace(source))
	if src != "mirror" {
		src = "github"
	}

	u.mu.Lock()
	if u.upgrade.State == "downloading" || u.upgrade.State == "verifying" || u.upgrade.State == "installing" || u.upgrade.State == "restarting" {
		cur := u.upgrade
		u.mu.Unlock()
		return cur, errConflict("upgrade_in_progress", "已有升级任务正在进行中")
	}
	ver := strings.TrimSpace(targetVersion)
	if ver == "" {
		ver = u.latest
	}
	if ver == "" {
		u.mu.Unlock()
		return u.progress(), errBad("version", "尚未检测到新版本，请先点击「检查更新」")
	}
	if !version.IsNewer(ver, currentAppVersion()) && os.Getenv("NFR_UPDATE_ALLOW_SAME_VERSION") != "1" {
		u.mu.Unlock()
		return u.progress(), errBad("version", fmt.Sprintf("当前版本 %s 已是最新，无需升级", currentAppVersion()))
	}

	initProg := api.UpgradeProgress{
		State:         "downloading",
		Percent:       5,
		TargetVersion: ver,
		Source:        src,
		Message:       "正在准备下载更新包…",
	}
	u.upgrade = initProg
	u.mu.Unlock()

	go u.runUpgrade(exePath, ver, src, mirrorPrefix)
	return initProg, nil
}

func (u *updater) runUpgrade(exePath, ver, source, mirrorPrefix string) {
	var checksumSource string
	fail := func(msg string, err error) {
		detail := msg
		if err != nil {
			detail = msg + ": " + err.Error()
		}
		u.srv.lg.Warn("self-upgrade failed", "target", ver, "err", detail)
		u.setProgress(api.UpgradeProgress{
			State:          "error",
			Percent:        0,
			TargetVersion:  ver,
			Source:         source,
			ChecksumSource: checksumSource,
			Message:        detail,
			Error:          detail,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	archiveName, err := releaseArchiveName(ver, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fail("无法匹配当前平台安装包", err)
		return
	}

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("NFR_UPDATE_DOWNLOAD_BASE")), "/")
	if baseURL == "" {
		baseURL = defaultRepoURL + "/releases/download/" + ver
	}
	rawArchiveURL := baseURL + "/" + archiveName
	rawSumsURL := baseURL + "/SHA256SUMS"

	var archiveURL, sumsURL string
	if source == "mirror" {
		var chosen api.MirrorNode
		if m, ok := validateAllowedMirror(mirrorPrefix); ok {
			chosen = m
		} else {
			u.setProgress(api.UpgradeProgress{
				State:         "downloading",
				Percent:       8,
				TargetVersion: ver,
				Source:        source,
				Message:       "正在测速优选镜像节点…",
			})
			probe := u.probeMirrors(ctx, false)
			chosen = probe.Mirror
		}
		if chosen.Prefix == "" {
			chosen = EffectiveMirrors()[0]
		}
		archiveURL = applyMirrorPrefix(chosen.Prefix, rawArchiveURL)
		sumsURL = applyMirrorPrefix(chosen.Prefix, rawSumsURL)
	} else {
		archiveURL = rawArchiveURL
		sumsURL = rawSumsURL
	}

	dir := filepath.Dir(exePath)
	tmpDir, err := os.MkdirTemp(dir, ".nfr-upgrade-*")
	if err != nil {
		fail("创建临时目录失败", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	// 1. Download SHA256SUMS.
	// When downloading the binary from a third-party acceleration mirror, try
	// fetching SHA256SUMS from the official GitHub source first so a mirror
	// cannot forge both the archive and its checksum; fall back to the mirror's
	// SHA256SUMS only if the official endpoint is unreachable from this host.
	u.setProgress(api.UpgradeProgress{
		State:         "downloading",
		Percent:       12,
		TargetVersion: ver,
		Source:        source,
		Message:       "正在获取校验清单 SHA256SUMS…",
	})
	var expectedHash string
	officialSumsURL := strings.TrimSpace(os.Getenv("NFR_UPDATE_OFFICIAL_SUMS_URL"))
	if officialSumsURL == "" {
		officialSumsURL = rawSumsURL
	}
	if source == "mirror" && officialSumsURL != sumsURL {
		officialCtx, officialCancel := context.WithTimeout(ctx, 8*time.Second)
		h, oerr := downloadExpectedSHA256(officialCtx, officialSumsURL, archiveName)
		officialCancel()
		if oerr == nil {
			expectedHash = h
			checksumSource = "official"
			u.srv.lg.Info("verified SHA256SUMS from official source for mirror upgrade", "target", ver)
		} else {
			u.srv.lg.Warn("official SHA256SUMS unreachable, falling back to mirror", "target", ver, "err", oerr)
		}
	}
	if expectedHash == "" {
		expectedHash, err = downloadExpectedSHA256(ctx, sumsURL, archiveName)
		if err != nil {
			fail("获取 SHA256SUMS 校验清单失败", err)
			return
		}
		if source == "mirror" {
			checksumSource = "mirror"
		} else {
			checksumSource = "official"
		}
	}

	// 2. Download archive with progress reporting
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := downloadFileWithProgress(ctx, archiveURL, archivePath, func(pct int) {
		// Map download 0..100% into overall 15..70%
		overall := 15 + (pct*55)/100
		u.setProgress(api.UpgradeProgress{
			State:          "downloading",
			Percent:        overall,
			TargetVersion:  ver,
			Source:         source,
			ChecksumSource: checksumSource,
			Message:        fmt.Sprintf("正在下载 %s (%d%%)…", archiveName, pct),
		})
	}); err != nil {
		fail("下载更新包失败", err)
		return
	}

	// 3. Verify SHA-256
	verifyMsg := "正在校验安装包完整性（GitHub 官方 SHA256SUMS）…"
	if checksumSource == "mirror" {
		verifyMsg = "正在校验安装包完整性（官方不可达，已回退镜像 SHA256SUMS）…"
	}
	u.setProgress(api.UpgradeProgress{
		State:          "verifying",
		Percent:        75,
		TargetVersion:  ver,
		Source:         source,
		ChecksumSource: checksumSource,
		Message:        verifyMsg,
	})
	actualHash, err := fileSHA256(archivePath)
	if err != nil {
		fail("计算文件哈希失败", err)
		return
	}
	if !strings.EqualFold(actualHash, expectedHash) {
		fail("SHA-256 校验不匹配，已中止更新", fmt.Errorf("期望 %s，实际 %s", expectedHash, actualHash))
		return
	}

	// 4. Extract binaries
	u.setProgress(api.UpgradeProgress{
		State:          "installing",
		Percent:        82,
		TargetVersion:  ver,
		Source:         source,
		ChecksumSource: checksumSource,
		Message:        "正在解压并验证新版本程序…",
	})
	nfrsBinName := "nfrs"
	nfrcBinName := "nfrc"
	if runtime.GOOS == "windows" {
		nfrsBinName = "nfrs.exe"
		nfrcBinName = "nfrc.exe"
	}
	newNfrsPath := filepath.Join(tmpDir, nfrsBinName+".new")
	newNfrcPath := filepath.Join(tmpDir, nfrcBinName+".new")
	hasNewNfrc, err := extractBinariesFromArchive(archivePath, nfrsBinName, newNfrsPath, nfrcBinName, newNfrcPath)
	if err != nil {
		fail("解压更新包失败", err)
		return
	}

	// 5. Smoke-test new nfrs binary before replacing
	smokeCtx, smokeCancel := context.WithTimeout(ctx, 5*time.Second)
	out, err := exec.CommandContext(smokeCtx, newNfrsPath, "version").CombinedOutput()
	smokeCancel()
	if err != nil {
		fail("新版本程序自检失败，已保持原版本不变", fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out))))
		return
	}

	// 6. Backup current binary and atomically replace
	u.setProgress(api.UpgradeProgress{
		State:          "installing",
		Percent:        92,
		TargetVersion:  ver,
		Source:         source,
		ChecksumSource: checksumSource,
		Message:        "正在原子替换服务端程序…",
	})
	if err := atomicReplaceBinary(exePath, newNfrsPath); err != nil {
		fail("替换 nfrs 二进制失败", err)
		return
	}
	// Also update sibling nfrc binary if it already exists in the same directory.
	if hasNewNfrc {
		siblingNfrc := filepath.Join(dir, nfrcBinName)
		if info, err := os.Stat(siblingNfrc); err == nil && !info.IsDir() {
			if err := atomicReplaceBinary(siblingNfrc, newNfrcPath); err != nil {
				u.srv.lg.Warn("sibling nfrc upgrade skipped", "path", siblingNfrc, "err", err)
			}
		}
	}

	// Clean up downloaded archive & temp dir explicitly BEFORE restarting/exec'ing
	// so neither archives nor empty .nfr-upgrade-* folders linger after restart.
	_ = os.RemoveAll(tmpDir)

	// 7. Signal restarting so frontend shows smooth reload state, then restart.
	u.setProgress(api.UpgradeProgress{
		State:          "restarting",
		Percent:        100,
		TargetVersion:  ver,
		Source:         source,
		ChecksumSource: checksumSource,
		Message:        fmt.Sprintf("已更新至 %s，正在平滑重启服务…", ver),
	})
	u.srv.lg.Info("self-upgrade applied, restarting", "version", ver, "path", exePath)

	u.mu.Lock()
	hook := u.restartHook
	u.mu.Unlock()
	if hook != nil {
		hook(exePath)
		return
	}

	time.Sleep(500 * time.Millisecond)
	u.restartProcess(exePath)
}

func (u *updater) restartProcess(exePath string) {
	// Under systemd (Restart=always), graceful shutdown lets systemd respawn
	// the freshly replaced binary cleanly.
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "" {
		if u.srv.cancel != nil {
			u.srv.cancel()
		}
		return
	}
	// Standalone Unix process: shut down listeners cleanly then exec the new binary.
	if u.srv.cancel != nil {
		u.srv.cancel()
	}
	time.Sleep(400 * time.Millisecond)
	if err := execSelf(exePath, os.Args); err != nil {
		u.srv.lg.Warn("execSelf failed after upgrade", "err", err)
		os.Exit(0)
	}
}

func downloadExpectedSHA256(ctx context.Context, sumsURL, archiveName string) (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "nfr-updater/"+currentAppVersion())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxMetadataDownloadBytes))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = filepath.Base(name)
		if name == archiveName && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中未找到 %s 的校验值", archiveName)
}

func downloadFileWithProgress(ctx context.Context, rawURL, dstPath string, onProgress func(int)) error {
	client := &http.Client{Timeout: 4 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nfr-updater/"+currentAppVersion())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	total := resp.ContentLength
	var written int64
	lastPct := -1
	buf := make([]byte, 32*1024)
	r := io.LimitReader(resp.Body, maxArchiveDownloadBytes+1)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxArchiveDownloadBytes {
				return errors.New("安装包体积超过安全上限")
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			if total > 0 && onProgress != nil {
				pct := int((written * 100) / total)
				if pct > 100 {
					pct = 100
				}
				if pct != lastPct && (pct-lastPct >= 5 || pct == 100) {
					lastPct = pct
					onProgress(pct)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if onProgress != nil && lastPct < 100 {
		onProgress(100)
	}
	return f.Sync()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractBinariesFromArchive(archivePath, nfrsName, nfrsDst, nfrcName, nfrcDst string) (hasNfrc bool, err error) {
	if strings.HasSuffix(archivePath, ".zip") {
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return false, err
		}
		defer zr.Close()
		var gotNfrs bool
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			base := filepath.Base(f.Name)
			var dst string
			switch base {
			case nfrsName:
				dst = nfrsDst
				gotNfrs = true
			case nfrcName:
				dst = nfrcDst
				hasNfrc = true
			default:
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return false, err
			}
			err = writeExecutableFile(dst, rc)
			_ = rc.Close()
			if err != nil {
				return false, err
			}
		}
		if !gotNfrs {
			return false, fmt.Errorf("压缩包中未找到 %s", nfrsName)
		}
		return hasNfrc, nil
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var gotNfrs bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		var dst string
		switch base {
		case nfrsName:
			dst = nfrsDst
			gotNfrs = true
		case nfrcName:
			dst = nfrcDst
			hasNfrc = true
		default:
			continue
		}
		if err := writeExecutableFile(dst, tr); err != nil {
			return false, err
		}
	}
	if !gotNfrs {
		return false, fmt.Errorf("压缩包中未找到 %s", nfrsName)
	}
	return hasNfrc, nil
}

func writeExecutableFile(dst string, r io.Reader) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(r, maxArchiveDownloadBytes)); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func atomicReplaceBinary(targetPath, newPath string) error {
	backupPath := targetPath + ".bak"
	// Best-effort copy backup of old binary before atomic swap.
	if oldBytes, err := os.ReadFile(targetPath); err == nil {
		_ = os.WriteFile(backupPath, oldBytes, 0o755)
	}
	// Stage into target directory (tmpDir is already inside target's directory)
	// and atomically rename into place.
	if err := os.Chmod(newPath, 0o755); err != nil {
		return err
	}
	return os.Rename(newPath, targetPath)
}

// Public Server methods exposed to internal/panel.

// UpdateStatus returns the current version & cached update status.
func (s *Server) UpdateStatus() api.UpdateStatus {
	if s.updater == nil {
		return api.UpdateStatus{CurrentVersion: currentAppVersion(), Mirrors: EffectiveMirrors()}
	}
	return s.updater.status()
}

// CheckForUpdates actively checks for updates via GitHub or mirror.
func (s *Server) CheckForUpdates(ctx context.Context, source, channel string) (api.UpdateStatus, error) {
	if s.updater == nil {
		return s.UpdateStatus(), errors.New("updater unavailable")
	}
	return s.updater.check(ctx, source, channel)
}

// ProbeUpdateMirrors measures mirror latency from the server host.
func (s *Server) ProbeUpdateMirrors(ctx context.Context, force bool) api.UpdateMirrorProbe {
	if s.updater == nil {
		return api.UpdateMirrorProbe{OK: false, Message: "updater unavailable"}
	}
	return s.updater.probeMirrors(ctx, force)
}

// SetUpdateSettings persists auto-check toggle and update channel.
func (s *Server) SetUpdateSettings(autoCheck bool, channel string) error {
	ch := normalizeChannel(channel)
	return s.st.Update(func(st *store.State) error {
		st.Settings.DisableAutoUpdateCheck = !autoCheck
		st.Settings.UpdateChannel = ch
		return nil
	})
}

// StartSelfUpgrade begins an asynchronous verified self-upgrade of nfrs.
func (s *Server) StartSelfUpgrade(source, mirrorPrefix, targetVersion string) (api.UpgradeProgress, error) {
	if s.updater == nil {
		return api.UpgradeProgress{}, errors.New("updater unavailable")
	}
	return s.updater.startUpgrade(source, mirrorPrefix, targetVersion)
}

// UpgradeProgress returns the live progress of an active or recent self-upgrade.
func (s *Server) UpgradeProgress() api.UpgradeProgress {
	if s.updater == nil {
		return api.UpgradeProgress{State: "idle"}
	}
	return s.updater.progress()
}

// LatestClientVersion returns the newest known version to advertise to connecting nfrc agents.
func (s *Server) LatestClientVersion() string {
	if s.updater == nil {
		return ""
	}
	return s.updater.latestClientVersion()
}
