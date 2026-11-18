package nodejs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// WorkspacePatterns 汇总 package.json workspaces、pnpm-workspace.yaml、lerna.json 里的成员模式。
func WorkspacePatterns(dir string, pkg *PackageJSON) []string {
	var pats []string
	if pkg != nil {
		pats = append(pats, pkg.Workspaces...)
	}
	pats = append(pats, pnpmWorkspacePatterns(sdk.ReadText(filepath.Join(dir, "pnpm-workspace.yaml"), 256<<10))...)
	if b, err := os.ReadFile(filepath.Join(dir, "lerna.json")); err == nil {
		var l struct {
			Packages []string `json:"packages"`
		}
		if json.Unmarshal(b, &l) == nil {
			if len(l.Packages) == 0 && len(pats) == 0 {
				l.Packages = []string{"packages/*"} // lerna 默认
			}
			pats = append(pats, l.Packages...)
		}
	}
	return pats
}

var yamlListItemRe = regexp.MustCompile(`^\s*-\s*['"]?([^'"#]+?)['"]?\s*(?:#.*)?$`)

// pnpmWorkspacePatterns 读取 pnpm-workspace.yaml 的 packages 列表（只解析这一段）。
func pnpmWorkspacePatterns(text string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimRight(line, "\r")
		if strings.TrimSpace(t) == "" || strings.HasPrefix(strings.TrimSpace(t), "#") {
			continue
		}
		if !strings.HasPrefix(t, " ") && !strings.HasPrefix(t, "-") {
			in = strings.HasPrefix(t, "packages:")
			continue
		}
		if in {
			if m := yamlListItemRe.FindStringSubmatch(t); m != nil {
				out = append(out, strings.TrimSpace(m[1]))
			}
		}
	}
	return out
}

// ExpandWorkspaces 把成员模式展开成含 package.json 的相对目录（斜杠分隔、排序、去重）。支持 *、**、! 排除。
func ExpandWorkspaces(dir string, patterns []string) []string {
	include := map[string]bool{}
	var excludes []string
	for _, p := range patterns {
		p = strings.TrimSpace(strings.TrimPrefix(filepath.ToSlash(p), "./"))
		if p == "" || p == "." {
			continue
		}
		if strings.HasPrefix(p, "!") {
			excludes = append(excludes, strings.TrimPrefix(strings.TrimPrefix(p, "!"), "./"))
			continue
		}
		for _, m := range globDirs(dir, p) {
			include[m] = true
		}
	}
	var out []string
	for m := range include {
		skip := false
		for _, ex := range excludes {
			if globMatch(ex, m) {
				skip = true
				break
			}
		}
		if !skip && sdk.FileExists(filepath.Join(dir, filepath.FromSlash(m), "package.json")) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// globDirs 展开一个目录模式。
func globDirs(root, pattern string) []string {
	segs := strings.Split(strings.TrimSuffix(pattern, "/"), "/")
	var walk func(base string, i int) []string
	walk = func(base string, i int) []string {
		if i == len(segs) {
			return []string{base}
		}
		seg := segs[i]
		abs := filepath.Join(root, filepath.FromSlash(base))
		if seg == "**" {
			// 零层或多层
			out := walk(base, i+1)
			entries, _ := os.ReadDir(abs)
			for _, e := range entries {
				if e.IsDir() && !skipWorkspaceDir(e.Name()) {
					out = append(out, walk(joinRel(base, e.Name()), i)...)
				}
			}
			return out
		}
		if !strings.ContainsAny(seg, "*?[") {
			next := joinRel(base, seg)
			if sdk.DirExists(filepath.Join(root, filepath.FromSlash(next))) {
				return walk(next, i+1)
			}
			return nil
		}
		var out []string
		entries, _ := os.ReadDir(abs)
		for _, e := range entries {
			if e.IsDir() && !skipWorkspaceDir(e.Name()) {
				if ok, _ := filepath.Match(seg, e.Name()); ok {
					out = append(out, walk(joinRel(base, e.Name()), i+1)...)
				}
			}
		}
		return out
	}
	return walk("", 0)
}

func skipWorkspaceDir(name string) bool {
	return name == "node_modules" || strings.HasPrefix(name, ".")
}

func joinRel(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

// globMatch 支持 ** 的简易匹配（用于排除模式）。
func globMatch(pattern, path string) bool {
	ps := strings.Split(strings.TrimSuffix(pattern, "/"), "/")
	xs := strings.Split(path, "/")
	var match func(i, j int) bool
	match = func(i, j int) bool {
		if i == len(ps) {
			return j == len(xs)
		}
		if ps[i] == "**" {
			for k := j; k <= len(xs); k++ {
				if match(i+1, k) {
					return true
				}
			}
			return false
		}
		if j == len(xs) {
			return false
		}
		if ok, _ := filepath.Match(ps[i], xs[j]); !ok {
			return false
		}
		return match(i+1, j+1)
	}
	return match(0, 0)
}
