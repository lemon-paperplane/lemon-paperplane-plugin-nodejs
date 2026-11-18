package nodejs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const officialNodeDist = "https://nodejs.org/dist"

// indexCacheTTL 内的缓存直接用；过期后联网刷新，失败再退回旧缓存。
const indexCacheTTL = 6 * time.Hour

// NodeRelease 是 index.json 里的一个版本。
type NodeRelease struct {
	Version string   `json:"version"` // "v22.20.0"
	Date    string   `json:"date"`
	Files   []string `json:"files"`
	LTS     ltsField `json:"lts"`
}

// ltsField 兼容 false 与代号字符串。
type ltsField string

func (l *ltsField) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = ltsField(s)
		return nil
	}
	*l = ""
	return nil
}

// Ver 返回去掉 v 的版本号。
func (r NodeRelease) Ver() string { return strings.TrimPrefix(r.Version, "v") }

// Stable 判断是否正式版（排除 alpha/rc/nightly）。
func (r NodeRelease) Stable() bool {
	v, ok := sdk.ParseVersion(r.Ver())
	return ok && v.Pre == "" && !strings.Contains(r.Version, "-")
}

func distBases(m sdk.Mirrors) []string {
	var out []string
	if m.NodeDist != "" {
		out = append(out, strings.TrimSuffix(m.NodeDist, "/"))
	}
	if len(out) == 0 || out[0] != officialNodeDist {
		out = append(out, officialNodeDist)
	}
	return out
}

func indexCachePath(cacheDir string) string { return filepath.Join(cacheDir, "node-index.json") }

// LoadCachedIndex 读取缓存的发行索引（不联网）。
func LoadCachedIndex(cacheDir string) ([]NodeRelease, time.Time) {
	if cacheDir == "" {
		return nil, time.Time{}
	}
	p := indexCachePath(cacheDir)
	st, err := os.Stat(p)
	if err != nil {
		return nil, time.Time{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, time.Time{}
	}
	var rs []NodeRelease
	if json.Unmarshal(b, &rs) != nil {
		return nil, time.Time{}
	}
	return rs, st.ModTime()
}

// FetchIndex 取发行索引：新鲜缓存 → 镜像 → 官方 → 旧缓存。
func FetchIndex(ctx context.Context, m sdk.Mirrors, cacheDir string, force bool) ([]NodeRelease, error) {
	cached, at := LoadCachedIndex(cacheDir)
	if !force && cached != nil && time.Since(at) < indexCacheTTL {
		return cached, nil
	}
	var lastErr error
	for _, base := range distBases(m) {
		b, err := httpGet(ctx, base+"/index.json", 20*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		var rs []NodeRelease
		if err := json.Unmarshal(b, &rs); err != nil || len(rs) == 0 {
			lastErr = fmt.Errorf("index.json: %v", err)
			continue
		}
		if cacheDir != "" {
			_ = os.MkdirAll(cacheDir, 0o755)
			tmp := indexCachePath(cacheDir) + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, indexCachePath(cacheDir))
				saveLTS(cacheDir, LTSFromIndex(rs))
			}
		}
		return rs, nil
	}
	if cached != nil {
		return cached, nil
	}
	return nil, lastErr
}

func httpGet(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LemonPaperplane")
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func ltsCachePath(cacheDir string) string { return filepath.Join(cacheDir, "node-lts.json") }

// LoadLTS 读取 LTS 线信息：优先读精简缓存（几百字节），没有时从发行索引推导并写入精简缓存，都没有用内置表。
// analyze 每次都要用，不能每次解码整个发行索引。
func LoadLTS(cacheDir string) LTSInfo {
	if cacheDir == "" {
		return builtinLTS()
	}
	var c struct {
		Majors   []int          `json:"majors"`
		Codename map[string]int `json:"codename"`
	}
	ls, err1 := os.Stat(ltsCachePath(cacheDir))
	is, err2 := os.Stat(indexCachePath(cacheDir))
	if err1 == nil && (err2 != nil || !is.ModTime().After(ls.ModTime())) {
		if b, err := os.ReadFile(ltsCachePath(cacheDir)); err == nil && json.Unmarshal(b, &c) == nil && len(c.Majors) > 0 {
			return LTSInfo{Majors: c.Majors, Codename: c.Codename}
		}
	}
	if err2 != nil {
		return builtinLTS()
	}
	rs, _ := LoadCachedIndex(cacheDir)
	info := LTSFromIndex(rs)
	saveLTS(cacheDir, info)
	return info
}

func saveLTS(cacheDir string, info LTSInfo) {
	b, err := json.Marshal(map[string]any{"majors": info.Majors, "codename": info.Codename})
	if err == nil {
		_ = os.WriteFile(ltsCachePath(cacheDir), b, 0o644)
	}
}

// LTSFromIndex 由发行索引得到 LTS 线信息；索引为空时用内置表。
func LTSFromIndex(rs []NodeRelease) LTSInfo {
	info := builtinLTS()
	if len(rs) == 0 {
		return info
	}
	majors := map[int]bool{}
	for _, r := range rs {
		if r.LTS == "" || !r.Stable() {
			continue
		}
		v, _ := sdk.ParseVersion(r.Ver())
		majors[v.Major()] = true
		info.Codename[strings.ToLower(string(r.LTS))] = v.Major()
	}
	if len(majors) == 0 {
		return info
	}
	info.Majors = info.Majors[:0]
	for m := range majors {
		info.Majors = append(info.Majors, m)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(info.Majors)))
	return info
}

// platformKey 返回 index.json files 字段里的平台键与下载文件名后缀。
type platformBuild struct {
	key  string // index.json files 里的键，如 osx-arm64-tar
	file string // 文件名中的平台段，如 darwin-arm64
	ext  string // tar.gz | zip
	arch string
}

func platformBuilds(goos, goarch string) []platformBuild {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "386": "x86", "arm": "armv7l", "ppc64le": "ppc64le", "s390x": "s390x"}[goarch]
	if arch == "" {
		arch = goarch
	}
	var out []platformBuild
	switch goos {
	case "darwin":
		out = append(out, platformBuild{"osx-" + arch + "-tar", "darwin-" + arch, "tar.gz", goarch})
		if arch == "arm64" {
			// 老版本没有 arm64 构建，Rosetta 下跑 x64
			out = append(out, platformBuild{"osx-x64-tar", "darwin-x64", "tar.gz", "amd64"})
		}
	case "windows":
		out = append(out, platformBuild{"win-" + arch + "-zip", "win-" + arch, "zip", goarch})
		if arch == "arm64" {
			out = append(out, platformBuild{"win-x64-zip", "win-x64", "zip", "amd64"})
		}
	default:
		out = append(out, platformBuild{"linux-" + arch, "linux-" + arch, "tar.gz", goarch})
	}
	return out
}

func buildFor(r NodeRelease, goos, goarch string) (platformBuild, bool) {
	for _, b := range platformBuilds(goos, goarch) {
		for _, f := range r.Files {
			if f == b.key {
				return b, true
			}
		}
	}
	return platformBuild{}, false
}

// AvailableVersions 每个主版本取最新正式版（偶数主版本全部列出，奇数只列比最新 LTS 新的 Current）。
func AvailableVersions(rs []NodeRelease, goos, goarch string) []sdk.AvailableVersion {
	lts := LTSFromIndex(rs)
	latest := map[int]NodeRelease{}
	for _, r := range rs {
		if !r.Stable() {
			continue
		}
		if _, ok := buildFor(r, goos, goarch); !ok {
			continue
		}
		v, _ := sdk.ParseVersion(r.Ver())
		m := v.Major()
		if m < 10 {
			continue
		}
		if cur, ok := latest[m]; !ok || sdk.CompareVersions(r.Ver(), cur.Ver()) > 0 {
			latest[m] = r
		}
	}
	var majors []int
	for m := range latest {
		if m%2 == 0 || m > lts.Latest() {
			majors = append(majors, m)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(majors)))
	var out []sdk.AvailableVersion
	for _, m := range majors {
		r := latest[m]
		av := sdk.AvailableVersion{Version: r.Ver(), Vendor: "Node.js", LTS: r.LTS != ""}
		av.Label = r.Ver()
		if r.LTS != "" {
			av.Label += " LTS (" + string(r.LTS) + ")"
		}
		av.Recommended = m == lts.Latest()
		out = append(out, av)
	}
	return out
}

// ResolveRelease 把具体版本、主版本、约束或 nvm 别名解析成索引里的一个版本。
func ResolveRelease(rs []NodeRelease, spec, goos, goarch string) (NodeRelease, platformBuild, bool) {
	lts := LTSFromIndex(rs)
	constraint, preferred := PinConstraint(spec, lts)
	s := strings.TrimPrefix(strings.TrimSpace(spec), "v")
	if v, ok := sdk.ParseVersion(s); ok && v.N >= 3 && !strings.ContainsAny(s, "<>^~|* ") {
		constraint = v.String()
	} else if constraint == "" && preferred != "" {
		constraint = preferred
	} else if constraint == "" && preferred == "" && !strings.ContainsAny(strings.ToLower(s), "abcdefghijklmnopqrstuvwxyz") && s != "" {
		constraint = s
	}
	wantLTS := strings.HasPrefix(strings.ToLower(s), "lts")
	var best NodeRelease
	var bestB platformBuild
	found := false
	for _, r := range rs {
		if !r.Stable() || (wantLTS && r.LTS == "") {
			continue
		}
		if constraint != "" && !sdk.MatchVersion(constraint, r.Ver()) {
			continue
		}
		b, ok := buildFor(r, goos, goarch)
		if !ok {
			continue
		}
		if !found || sdk.CompareVersions(r.Ver(), best.Ver()) > 0 {
			best, bestB, found = r, b, true
		}
	}
	return best, bestB, found
}

// InstallPlanFor 生成下载计划：镜像在前，sha256 取自 SHASUMS256.txt。
func InstallPlanFor(ctx context.Context, m sdk.Mirrors, r NodeRelease, b platformBuild) *sdk.InstallPlan {
	ver := r.Ver()
	file := fmt.Sprintf("node-v%s-%s.%s", ver, b.file, b.ext)
	plan := &sdk.InstallPlan{Version: ver, Archive: b.ext, StripComponents: 1, FileName: file}
	for _, base := range distBases(m) {
		plan.URLs = append(plan.URLs, fmt.Sprintf("%s/v%s/%s", base, ver, file))
	}
	for _, base := range distBases(m) {
		body, err := httpGet(ctx, fmt.Sprintf("%s/v%s/SHASUMS256.txt", base, ver), 15*time.Second)
		if err != nil {
			continue
		}
		if sum := shaFor(string(body), file); sum != "" {
			plan.SHA256 = sum
			break
		}
	}
	return plan
}

func shaFor(sums, file string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == file && len(f[0]) == 64 {
			return f[0]
		}
	}
	return ""
}

// itoa 便于拼接主版本。
func itoa(n int) string { return strconv.Itoa(n) }
