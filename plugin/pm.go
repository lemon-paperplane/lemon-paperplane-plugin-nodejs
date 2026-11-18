package nodejs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 包管理器名。
const (
	PMNpm  = "npm"
	PMYarn = "yarn"
	PMPnpm = "pnpm"
	PMBun  = "bun"
)

// lockfileManagers 是锁文件 → 包管理器。
var lockfileManagers = []struct{ file, pm string }{
	{"pnpm-lock.yaml", PMPnpm},
	{"yarn.lock", PMYarn},
	{"package-lock.json", PMNpm},
	{"npm-shrinkwrap.json", PMNpm},
	{"bun.lock", PMBun},
	{"bun.lockb", PMBun},
}

// installMarkers 是各包管理器装完依赖后在 node_modules 里留下的标记文件，用来判断「最近是谁装的」。
var installMarkers = map[string][]string{
	PMNpm:  {"node_modules/.package-lock.json"},
	PMYarn: {"node_modules/.yarn-integrity", "node_modules/.yarn-state.yml", ".yarn/install-state.gz"},
	PMPnpm: {"node_modules/.modules.yaml"},
	PMBun:  {"node_modules/.bun-tag"},
}

// PMInfo 是识别出的包管理器。
type PMInfo struct {
	Name      string   `json:"name"`              // npm | yarn | pnpm | bun
	Version   string   `json:"version,omitempty"` // packageManager 字段或 yarnPath 给出的精确版本
	Exact     bool     `json:"exact,omitempty"`   // Version 是 packageManager 钉死的
	Major     string   `json:"major,omitempty"`   // 推断出的主版本（如 pnpm 锁文件格式对应的版本）
	Berry     bool     `json:"berry,omitempty"`   // yarn 2+
	YarnPath  string   `json:"yarnPath,omitempty"`
	Lockfile  string   `json:"lockfile,omitempty"`  // 采用的锁文件
	Lockfiles []string `json:"lockfiles,omitempty"` // 目录下所有锁文件
	Source    string   `json:"source,omitempty"`    // packageManager | lockfile | install | engines | default
}

var pmFieldRe = regexp.MustCompile(`^(npm|yarn|pnpm|bun)@([^+\s]+)`)

// DetectPM 识别 dir 使用的包管理器。顺序：packageManager 字段 → 锁文件（多个时看 node_modules 里最近一次安装的标记，再看锁文件修改时间）→ .yarnrc.yml → engines → npm。
func DetectPM(dir string, pkg *PackageJSON) PMInfo {
	info := PMInfo{}
	for _, l := range lockfileManagers {
		if sdk.FileExists(filepath.Join(dir, l.file)) {
			info.Lockfiles = append(info.Lockfiles, l.file)
		}
	}
	if pkg != nil && pkg.PackageManager != "" {
		if m := pmFieldRe.FindStringSubmatch(pkg.PackageManager); m != nil {
			info.Name, info.Version, info.Exact, info.Source = m[1], m[2], true, "packageManager"
		}
	}
	if info.Name == "" && len(info.Lockfiles) > 0 {
		info.Lockfile = pickLockfile(dir, info.Lockfiles)
		info.Name = lockfileManager(info.Lockfile)
		info.Source = "lockfile"
		if len(info.Lockfiles) > 1 {
			info.Source = "install"
		}
	}
	if info.Name == "" && (sdk.FileExists(filepath.Join(dir, ".yarnrc.yml")) || sdk.FileExists(filepath.Join(dir, ".yarnrc.yaml"))) {
		info.Name, info.Berry, info.Source = PMYarn, true, "yarnrc"
	}
	if info.Name == "" && pkg != nil {
		for _, pm := range []string{PMPnpm, PMYarn, PMBun} {
			if pkg.Engines[pm] != "" {
				info.Name, info.Source = pm, "engines"
				info.Major = majorOf(cleanRange(pkg.Engines[pm]))
				break
			}
		}
	}
	if info.Name == "" {
		info.Name, info.Source = PMNpm, "default"
	}
	// 选定包管理器后补全锁文件与版本信息
	if info.Lockfile == "" {
		for _, l := range lockfileManagers {
			if l.pm == info.Name && contains(info.Lockfiles, l.file) {
				info.Lockfile = l.file
				break
			}
		}
	}
	switch info.Name {
	case PMYarn:
		if info.Lockfile != "" {
			head := sdk.ReadText(filepath.Join(dir, info.Lockfile), 4096)
			if strings.Contains(head, "__metadata:") {
				info.Berry = true
			}
		}
		if yp := yarnPath(dir); yp != "" {
			info.YarnPath = yp
			info.Berry = true
			if info.Version == "" {
				if m := regexp.MustCompile(`yarn-(\d+\.\d+\.\d+[^/\\]*)\.c?js$`).FindStringSubmatch(yp); m != nil {
					info.Version = m[1]
				}
			}
		}
		if info.Version != "" && majorOf(info.Version) != "1" {
			info.Berry = true
		}
		if info.Major == "" {
			if info.Berry {
				info.Major = majorOf(info.Version)
			} else {
				info.Major = "1"
			}
		}
	case PMPnpm:
		if info.Major == "" && info.Lockfile != "" {
			info.Major = pnpmMajorFromLock(sdk.ReadText(filepath.Join(dir, info.Lockfile), 256))
		}
		if info.Major == "" {
			info.Major = majorOf(info.Version)
		}
	default:
		if info.Major == "" {
			info.Major = majorOf(info.Version)
		}
	}
	return info
}

func lockfileManager(file string) string {
	for _, l := range lockfileManagers {
		if l.file == file {
			return l.pm
		}
	}
	return PMNpm
}

// pickLockfile 在多个锁文件里选一个：优先看 node_modules 里哪个包管理器最近装过依赖，其次看锁文件修改时间。
func pickLockfile(dir string, files []string) string {
	if len(files) == 1 {
		return files[0]
	}
	best, bestT := "", time.Time{}
	for _, f := range files {
		pm := lockfileManager(f)
		for _, m := range installMarkers[pm] {
			if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(m))); err == nil && st.ModTime().After(bestT) {
				best, bestT = f, st.ModTime()
			}
		}
	}
	if best != "" {
		return best
	}
	for _, f := range files {
		if st, err := os.Stat(filepath.Join(dir, f)); err == nil && st.ModTime().After(bestT) {
			best, bestT = f, st.ModTime()
		}
	}
	return best
}

var yarnPathRe = regexp.MustCompile(`(?m)^yarnPath:\s*["']?([^"'\s]+)`)

func yarnPath(dir string) string {
	for _, f := range []string{".yarnrc.yml", ".yarnrc.yaml"} {
		if m := yarnPathRe.FindStringSubmatch(sdk.ReadText(filepath.Join(dir, f), 64<<10)); m != nil {
			return m[1]
		}
	}
	return ""
}

var pnpmLockRe = regexp.MustCompile(`lockfileVersion:\s*['"]?(\d+)(?:\.(\d+))?`)

// pnpmMajorFromLock 由锁文件格式推断 pnpm 主版本：5.3→6，5.4→7，6.x→8，9.0→9（pnpm 10 也写 9.0）。
func pnpmMajorFromLock(head string) string {
	m := pnpmLockRe.FindStringSubmatch(head)
	if m == nil {
		return ""
	}
	switch {
	case m[1] == "5" && m[2] == "3":
		return "6"
	case m[1] == "5":
		return "7"
	case m[1] == "6":
		return "8"
	case m[1] == "9":
		return "9"
	}
	return ""
}

func majorOf(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return ""
	}
	if i := strings.IndexAny(v, ".-+"); i >= 0 {
		v = v[:i]
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return v
}

// Display 返回界面上的包管理器文本，如 "yarn 1"、"pnpm 9.1.0"、"npm"。
func (i PMInfo) Display() string {
	switch {
	case i.Version != "":
		return i.Name + " " + i.Version
	case i.Name == PMYarn && i.Berry:
		return "yarn Berry"
	case i.Major != "" && i.Name != PMNpm:
		return i.Name + " " + i.Major
	}
	return i.Name
}

// Spec 返回 corepack / npm 安装时使用的 "<pm>@<版本>"。
func (i PMInfo) Spec() string {
	switch {
	case i.Version != "":
		return i.Name + "@" + i.Version
	case i.Name == PMYarn && i.Berry:
		return "yarn@stable"
	case i.Major != "":
		return i.Name + "@" + i.Major
	case i.Name == PMYarn:
		return "yarn@1"
	}
	return i.Name + "@latest"
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
