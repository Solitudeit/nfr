package panel

import (
	"net/http"
	"os"
	"strings"

	"github.com/solitudeit/nfr/internal/api"
	"github.com/solitudeit/nfr/internal/server"
	"github.com/solitudeit/nfr/internal/version"
)

const (
	defaultGitHubRepoURL = "https://github.com/Solitudeit/nfr"
	defaultReleaseTag    = "v0.13.0-preview.3"
)

// defaultGitHubDownloadBase resolves the GitHub Releases download base URL.
// Tagged release binaries (including pre-releases like v0.13.0-preview.3) point
// directly to /releases/download/<version> so downloads work reliably even when
// all releases are marked as pre-releases (where /releases/latest returns 404).
func defaultGitHubDownloadBase() string {
	v := strings.TrimSpace(version.Version)
	if strings.HasPrefix(v, "v") && !strings.HasSuffix(v, "-dirty") {
		return defaultGitHubRepoURL + "/releases/download/" + v
	}
	return defaultGitHubRepoURL + "/releases/download/" + defaultReleaseTag
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
	OS         string            `json:"os"`
	OSLabel    string            `json:"os_label"`
	Arch       string            `json:"arch"`
	ArchLabel  string            `json:"arch_label"`
	Filename   string            `json:"filename"`
	BinaryName string            `json:"binary_name"`
	URLs       map[string]string `json:"urls"`
}

func downloadBase(envKey, fallback string) string {
	v := strings.TrimRight(strings.TrimSpace(os.Getenv(envKey)), "/")
	if v != "" {
		return v
	}
	return fallback
}

func effectiveMirrors() []api.MirrorNode {
	return server.EffectiveMirrors()
}

// apiListDownloads returns external download links (GitHub + Mirror acceleration)
// for all 8 client platforms. Local dev direct download is disabled in production.
func (p *Panel) apiListDownloads(w http.ResponseWriter, r *http.Request) {
	githubBase := downloadBase("NFR_GITHUB_DOWNLOAD_BASE", defaultGitHubDownloadBase())
	mirrors := effectiveMirrors()
	fallbackMirrorBase := mirrors[0].Prefix + githubBase

	items := make([]downloadItem, 0, len(knownClientArchives))
	for _, spec := range knownClientArchives {
		githubURL := githubBase + "/" + spec.Filename
		items = append(items, downloadItem{
			OS:         spec.OS,
			OSLabel:    spec.OSLabel,
			Arch:       spec.Arch,
			ArchLabel:  spec.ArchLabel,
			Filename:   spec.Filename,
			BinaryName: spec.BinaryName,
			URLs: map[string]string{
				"github": githubURL,
				"mirror": mirrors[0].Prefix + githubURL,
			},
		})
	}

	sources := []downloadSource{
		{Key: "github", Label: "GitHub", BaseURL: githubBase},
		{Key: "mirror", Label: "镜像下载", BaseURL: fallbackMirrorBase},
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"default_source": "github",
		"sources":        sources,
		"mirrors":        mirrors,
		"items":          items,
	})
}
