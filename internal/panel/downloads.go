package panel

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/solitudeit/nfr/internal/version"
)

const (
	defaultGitHubRepoURL = "https://github.com/Solitudeit/nfr"
	defaultReleaseTag    = "v0.13.0-preview.1"
)

// defaultGitHubDownloadBase resolves the GitHub Releases download base URL.
// Tagged release binaries (including pre-releases like v0.13.0-preview.1) point
// directly to /releases/download/<version> so downloads work reliably even when
// all releases are marked as pre-releases (where /releases/latest returns 404).
func defaultGitHubDownloadBase() string {
	v := strings.TrimSpace(version.Version)
	if strings.HasPrefix(v, "v") && !strings.HasSuffix(v, "-dirty") {
		return defaultGitHubRepoURL + "/releases/download/" + v
	}
	return defaultGitHubRepoURL + "/releases/download/" + defaultReleaseTag
}

type mirrorNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	ProbeURL string `json:"probe_url"`
}

// defaultDownloadMirrors lists verified GitHub Release acceleration mirrors.
// The panel frontend races these directly from the user's browser and picks
// the lowest-latency reachable node for "镜像下载".
var defaultDownloadMirrors = []mirrorNode{
	{ID: "edgeone", Name: "EdgeOne 镜像", Prefix: "https://edgeone.gh-proxy.com/", ProbeURL: "https://edgeone.gh-proxy.com/"},
	{ID: "gh-proxy-org", Name: "gh-proxy.org", Prefix: "https://gh-proxy.org/", ProbeURL: "https://gh-proxy.org/"},
	{ID: "gh-proxy-com", Name: "gh-proxy.com", Prefix: "https://gh-proxy.com/", ProbeURL: "https://gh-proxy.com/"},
	{ID: "ghfast", Name: "ghfast.top", Prefix: "https://ghfast.top/", ProbeURL: "https://ghfast.top/"},
	{ID: "yylx", Name: "git.yylx.win", Prefix: "https://git.yylx.win/", ProbeURL: "https://git.yylx.win/"},
}

type clientSpec struct {
	OS         string
	OSLabel    string
	Arch       string
	ArchLabel  string
	Filename   string
	BinaryName string
}

// knownClientArchives is the exact allowlist of downloadable nfrc archives.
// Each archive is named by OS + arch and contains a single short-named binary
// ("nfrc" or "nfrc.exe") at its root.
var knownClientArchives = []clientSpec{
	{OS: "windows", OSLabel: "Windows", Arch: "amd64", ArchLabel: "64 位 (amd64)", Filename: "nfrc_windows_amd64.zip", BinaryName: "nfrc.exe"},
	{OS: "windows", OSLabel: "Windows", Arch: "arm64", ArchLabel: "ARM64", Filename: "nfrc_windows_arm64.zip", BinaryName: "nfrc.exe"},
	{OS: "linux", OSLabel: "Linux", Arch: "amd64", ArchLabel: "x86_64 (amd64)", Filename: "nfrc_linux_amd64.tar.gz", BinaryName: "nfrc"},
	{OS: "linux", OSLabel: "Linux", Arch: "arm64", ArchLabel: "ARM64", Filename: "nfrc_linux_arm64.tar.gz", BinaryName: "nfrc"},
	{OS: "linux", OSLabel: "Linux", Arch: "armv7", ArchLabel: "ARMv7 (树莓派/路由器)", Filename: "nfrc_linux_armv7.tar.gz", BinaryName: "nfrc"},
	{OS: "darwin", OSLabel: "macOS", Arch: "arm64", ArchLabel: "Apple Silicon (arm64)", Filename: "nfrc_macos_arm64.tar.gz", BinaryName: "nfrc"},
	{OS: "darwin", OSLabel: "macOS", Arch: "amd64", ArchLabel: "Intel (amd64)", Filename: "nfrc_macos_amd64.tar.gz", BinaryName: "nfrc"},
	{OS: "android", OSLabel: "Android", Arch: "arm64", ArchLabel: "ARM64 (Termux)", Filename: "nfrc_android_arm64.tar.gz", BinaryName: "nfrc"},
}

type downloadSource struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	BaseURL string `json:"base_url"`
}

type downloadItem struct {
	OS             string            `json:"os"`
	OSLabel        string            `json:"os_label"`
	Arch           string            `json:"arch"`
	ArchLabel      string            `json:"arch_label"`
	Filename       string            `json:"filename"`
	BinaryName     string            `json:"binary_name"`
	LocalAvailable bool              `json:"local_available"`
	SizeBytes      int64             `json:"size_bytes,omitempty"`
	URLs           map[string]string `json:"urls"`
}

func downloadBase(envKey, fallback string) string {
	v := strings.TrimRight(strings.TrimSpace(os.Getenv(envKey)), "/")
	if v != "" {
		return v
	}
	return fallback
}

func effectiveMirrors() []mirrorNode {
	raw := strings.TrimSpace(os.Getenv("NFR_MIRROR_PREFIXES"))
	if raw == "" {
		out := make([]mirrorNode, len(defaultDownloadMirrors))
		copy(out, defaultDownloadMirrors)
		return out
	}
	var out []mirrorNode
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
		out = append(out, mirrorNode{
			ID:       fmt.Sprintf("mirror-%d", i+1),
			Name:     label,
			Prefix:   p,
			ProbeURL: p,
		})
	}
	if len(out) == 0 {
		out = make([]mirrorNode, len(defaultDownloadMirrors))
		copy(out, defaultDownloadMirrors)
	}
	return out
}

func (p *Panel) apiListDownloads(w http.ResponseWriter, r *http.Request) {
	dir := p.srv.ClientsDir()
	githubBase := downloadBase("NFR_GITHUB_DOWNLOAD_BASE", defaultGitHubDownloadBase())
	mirrors := effectiveMirrors()
	fallbackMirrorBase := mirrors[0].Prefix + githubBase

	hasLocal := false
	items := make([]downloadItem, 0, len(knownClientArchives))
	for _, spec := range knownClientArchives {
		githubURL := githubBase + "/" + spec.Filename
		urls := map[string]string{
			"github": githubURL,
			"mirror": mirrors[0].Prefix + githubURL,
		}
		var size int64
		localOK := false
		if info, err := os.Stat(filepath.Join(dir, spec.Filename)); err == nil && !info.IsDir() {
			localOK = true
			hasLocal = true
			size = info.Size()
			urls["local"] = "/nfr/api/downloads/" + spec.Filename
		}
		items = append(items, downloadItem{
			OS:             spec.OS,
			OSLabel:        spec.OSLabel,
			Arch:           spec.Arch,
			ArchLabel:      spec.ArchLabel,
			Filename:       spec.Filename,
			BinaryName:     spec.BinaryName,
			LocalAvailable: localOK,
			SizeBytes:      size,
			URLs:           urls,
		})
	}

	sources := []downloadSource{
		{Key: "github", Label: "GitHub", BaseURL: githubBase},
		{Key: "mirror", Label: "镜像下载", BaseURL: fallbackMirrorBase},
	}
	if hasLocal {
		sources = append(sources, downloadSource{
			Key:     "local",
			Label:   "本机直下（开发）",
			BaseURL: "/nfr/api/downloads",
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"default_source": "github",
		"sources":        sources,
		"mirrors":        mirrors,
		"items":          items,
	})
}

func (p *Panel) apiDownloadClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("filename")
	var matched *clientSpec
	for i := range knownClientArchives {
		if knownClientArchives[i].Filename == name {
			matched = &knownClientArchives[i]
			break
		}
	}
	if matched == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "客户端压缩包不存在"})
		return
	}

	fullPath := filepath.Join(p.srv.ClientsDir(), matched.Filename)
	f, err := os.Open(fullPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "本机未生成该平台客户端压缩包"})
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "客户端文件不可用"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, matched.Filename))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, matched.Filename, info.ModTime(), f)
}
