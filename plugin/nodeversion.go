package nodejs

import (
	"bufio"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ltsCodenames 是 LTS 代号 → 主版本（离线兜底；有缓存的发行索引时以索引为准）。
var ltsCodenames = map[string]int{
	"argon": 4, "boron": 6, "carbon": 8, "dubnium": 10, "erbium": 12, "fermium": 14,
	"gallium": 16, "hydrogen": 18, "iron": 20, "jod": 22, "krypton": 24, "lithium": 26,
}

// builtinLatestLTS 是没有发行索引缓存时认为的最新 LTS 主版本。
const builtinLatestLTS = 26

// LTSInfo 是 LTS 线的信息。
type LTSInfo struct {
	Majors   []int          // 从新到旧
	Codename map[string]int // 小写代号 → 主版本
}

func builtinLTS() LTSInfo {
	info := LTSInfo{Codename: map[string]int{}}
	for k, v := range ltsCodenames {
		info.Codename[k] = v
	}
	for m := builtinLatestLTS; m >= 4; m -= 2 {
		info.Majors = append(info.Majors, m)
	}
	return info
}

// Latest 返回最新 LTS 主版本。
func (l LTSInfo) Latest() int {
	if len(l.Majors) == 0 {
		return builtinLatestLTS
	}
	return l.Majors[0]
}

// VersionPin 是工程里显式钉住的 Node 版本（.nvmrc 等）。
type VersionPin struct {
	Raw    string
	Source string // .nvmrc | .node-version | .tool-versions | mise.toml | package.json volta
	Dir    string
}

var (
	toolVersionsRe = regexp.MustCompile(`(?m)^\s*(?:nodejs|node)\s+([^\s#]+)`)
	miseTomlRe     = regexp.MustCompile(`(?m)^\s*(?:node|nodejs)\s*=\s*["']?([^"'\s#\]]+)`)
)

// FindVersionPin 从 dir 向上（不超过 root）找 .nvmrc / .node-version / .tool-versions / mise.toml，最后看 package.json 的 volta.node。
func FindVersionPin(dir, root string, pkg *PackageJSON) *VersionPin {
	if pkg != nil && pkg.Volta["node"] != "" {
		return &VersionPin{Raw: pkg.Volta["node"], Source: "package.json volta", Dir: dir}
	}
	d := filepath.Clean(dir)
	root = filepath.Clean(root)
	for {
		if v := firstLine(sdk.ReadText(filepath.Join(d, ".nvmrc"), 4096)); v != "" {
			return &VersionPin{Raw: v, Source: ".nvmrc", Dir: d}
		}
		if v := firstLine(sdk.ReadText(filepath.Join(d, ".node-version"), 4096)); v != "" {
			return &VersionPin{Raw: v, Source: ".node-version", Dir: d}
		}
		if m := toolVersionsRe.FindStringSubmatch(sdk.ReadText(filepath.Join(d, ".tool-versions"), 64<<10)); m != nil {
			return &VersionPin{Raw: m[1], Source: ".tool-versions", Dir: d}
		}
		for _, f := range []string{"mise.toml", ".mise.toml"} {
			if m := miseTomlRe.FindStringSubmatch(sdk.ReadText(filepath.Join(d, f), 64<<10)); m != nil {
				return &VersionPin{Raw: m[1], Source: f, Dir: d}
			}
		}
		if d == root || root == "" || !strings.HasPrefix(d, root) {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return nil
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if i := strings.Index(l, "#"); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		if l != "" {
			return l
		}
	}
	return ""
}

// PinConstraint 把钉住的版本（含 nvm 别名）转换成约束与偏好版本。
// "16" → ("16","16")；"v18.17.1" → ("18","18.17.1")；"lts/gallium" → ("16","16")；"lts/*" → ("", 最新 LTS)；
// "node"/"stable"/"latest" → ("","")；">=18" → (">=18","")。
func PinConstraint(raw string, lts LTSInfo) (constraint, preferred string) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "v")
	switch s {
	case "", "system", "iojs", "node", "stable", "latest", "current", "newest":
		return "", ""
	case "lts", "lts/*", "lts/latest", "--lts":
		return "", strconv.Itoa(lts.Latest())
	}
	if strings.HasPrefix(s, "lts/") {
		name := strings.TrimPrefix(s, "lts/")
		if strings.HasPrefix(name, "-") {
			if n, err := strconv.Atoi(name[1:]); err == nil && n < len(lts.Majors) {
				m := strconv.Itoa(lts.Majors[n])
				return m, m
			}
			return "", ""
		}
		if m, ok := lts.Codename[name]; ok {
			return strconv.Itoa(m), strconv.Itoa(m)
		}
		return "", ""
	}
	if strings.ContainsAny(s, "<>^~|* ") || strings.Contains(s, ".x") {
		if sdk.ParseConstraint(s).Any() {
			return "", ""
		}
		return s, ""
	}
	v, ok := sdk.ParseVersion(s)
	if !ok {
		return "", ""
	}
	return strconv.Itoa(v.Major()), v.String()
}

// nodeRule 是「某个依赖的某个版本段 → Node 版本要求」的经验规则。
type nodeRule struct {
	pkg        string
	versions   string // 依赖自身版本的约束
	constraint string // 对 Node 的约束
	prefer     string // 偏好的 Node 主版本
	legacy     bool   // webpack 4 时代：Node 17+ 需要 --openssl-legacy-provider
}

// nodeRules 只收录确定、影响大的组合；其余交给已安装依赖 / 锁文件里的 engines。
var nodeRules = []nodeRule{
	{pkg: "vite", versions: ">=7", constraint: "^20.19 || >=22.12"},
	{pkg: "vite", versions: "5 - 6", constraint: "^18 || >=20"},
	{pkg: "vite", versions: "3 - 4", constraint: "^14.18 || >=16"},
	{pkg: "vite", versions: "2", constraint: ">=12.2"},
	{pkg: "next", versions: ">=16", constraint: ">=20.9"},
	{pkg: "next", versions: "15", constraint: "^18.18 || ^19.8 || >=20"},
	{pkg: "next", versions: "14", constraint: ">=18.17"},
	{pkg: "next", versions: "13", constraint: ">=16.14"},
	{pkg: "next", versions: "12", constraint: ">=12.22"},
	{pkg: "next", versions: "<12", prefer: "16", legacy: true},
	{pkg: "nuxt", versions: ">=4", constraint: "^20.19 || >=22.12"},
	{pkg: "nuxt", versions: "3", constraint: ">=18"},
	{pkg: "nuxt", versions: "<3", prefer: "16", legacy: true},
	{pkg: "@vue/cli-service", versions: ">=5", constraint: ">=12"},
	{pkg: "@vue/cli-service", versions: "<5", prefer: "16", legacy: true},
	{pkg: "react-scripts", versions: ">=5", constraint: ">=14"},
	{pkg: "react-scripts", versions: "<5", prefer: "16", legacy: true},
	{pkg: "webpack", versions: "<5", prefer: "16", legacy: true},
	{pkg: "@dcloudio/vue-cli-plugin-uni", versions: "*", prefer: "16", legacy: true},
	// node-sass 只有为特定 Node 版本预编译的二进制，超出范围要本地编译，几乎必然失败
	{pkg: "node-sass", versions: "<5", constraint: "<15", prefer: "14"},
	{pkg: "node-sass", versions: "5", constraint: "<16", prefer: "14"},
	{pkg: "node-sass", versions: "6", constraint: "<17", prefer: "16"},
	{pkg: "node-sass", versions: "7", constraint: "<18", prefer: "16"},
	{pkg: "node-sass", versions: "8", constraint: "<20", prefer: "18"},
	{pkg: "node-sass", versions: ">=9", constraint: "<21", prefer: "20"},
}

// NodeHint 是一条推断依据。
type NodeHint struct {
	Constraint string `json:"constraint,omitempty"`
	Prefer     string `json:"prefer,omitempty"`
	Source     string `json:"source"`
	Legacy     bool   `json:"legacy,omitempty"`
	Advisory   bool   `json:"advisory,omitempty"` // 来自依赖 engines，冲突时可丢弃
}

// NodeRequirement 是最终的 Node 版本需求。
type NodeRequirement struct {
	Constraint   string     `json:"constraint,omitempty"`
	Preferred    string     `json:"preferred,omitempty"`
	Source       string     `json:"source,omitempty"`
	Legacy       bool       `json:"legacy,omitempty"`       // 需要 OpenSSL 旧版兼容（Node 17+ 时）
	LegacySource string     `json:"legacySource,omitempty"` // 如 "@vue/cli-service 4.5"
	Pinned       bool       `json:"pinned,omitempty"`       // 由 .nvmrc 等显式钉住
	Inferred     string     `json:"inferred,omitempty"`     // 依赖推断出的约束（与钉住版本冲突时用于修复）
	Conflict     bool       `json:"conflict,omitempty"`     // 钉住/声明的版本与依赖推断冲突
	Hints        []NodeHint `json:"hints,omitempty"`
}

// ResolveNodeRequirement 综合钉住版本、engines.node、依赖规则与依赖 engines 得出 Node 需求。
// versions 是直接依赖的实际版本（已安装优先，其次锁文件，最后声明范围的最低版本）；engines 是直接依赖声明的 engines.node。
func ResolveNodeRequirement(pkg *PackageJSON, pin *VersionPin, versions map[string]string, engines map[string]string, lts LTSInfo) NodeRequirement {
	var req NodeRequirement
	var hints []NodeHint
	// 依赖规则
	for _, r := range nodeRules {
		if pkg == nil || !pkg.HasDep(r.pkg) {
			continue
		}
		v := versions[r.pkg]
		if v == "" {
			v = cleanRange(pkg.DepRange(r.pkg))
		}
		if r.versions != "*" && (v == "" || !sdk.MatchVersion(r.versions, v)) {
			continue
		}
		src := r.pkg
		if mm := majorMinor(v); mm != "" {
			src += " " + mm
		}
		hints = append(hints, NodeHint{Constraint: r.constraint, Prefer: r.prefer, Source: src, Legacy: r.legacy})
	}
	// 依赖自己声明的 engines（参考性质）
	for _, name := range sortedKeys(engines) {
		c := strings.TrimSpace(engines[name])
		if c == "" || sdk.ParseConstraint(c).Any() {
			continue
		}
		hints = append(hints, NodeHint{Constraint: c, Source: name, Advisory: true})
	}
	req.Hints = hints

	// 先合并强规则，再逐个尝试参考性的 engines，冲突的丢弃
	inferred := ""
	var sources []string
	for _, h := range hints {
		if h.Legacy && !req.Legacy {
			req.Legacy, req.LegacySource = true, h.Source
		}
		if h.Constraint == "" {
			continue
		}
		c, ok := IntersectConstraints(inferred, h.Constraint)
		if !ok {
			continue
		}
		label := h.Source
		if h.Advisory {
			// 同一个包有带版本的规则时用规则的名字（"@vue/cli-service 4.5"）
			for _, o := range hints {
				if !o.Advisory && strings.Fields(o.Source)[0] == h.Source {
					label = o.Source
					break
				}
			}
		}
		if c != inferred && (!h.Advisory || len(sources) < 3) && !hasSourcePkg(sources, label) {
			sources = append(sources, label)
		}
		inferred = c
	}
	req.Inferred = inferred

	declared := ""
	declaredSrc := ""
	if pkg != nil && strings.TrimSpace(pkg.Engines["node"]) != "" {
		if c := strings.TrimSpace(pkg.Engines["node"]); !sdk.ParseConstraint(c).Any() && Satisfiable(c) {
			declared, declaredSrc = c, "package.json engines"
		}
	}

	switch {
	case pin != nil:
		c, p := PinConstraint(pin.Raw, lts)
		req.Pinned = true
		req.Constraint, req.Preferred, req.Source = c, p, pin.Source
		if c != "" && inferred != "" && !Satisfiable(c, inferred) {
			req.Conflict = true
		} else if c == "" && p != "" && inferred != "" && !Satisfiable(p, inferred) {
			req.Conflict = true
		}
	case declared != "":
		req.Source = declaredSrc
		if c, ok := IntersectConstraints(declared, inferred); ok {
			req.Constraint = c
			if c != declared && len(sources) > 0 {
				req.Source += ", " + strings.Join(sources, ", ")
			}
		} else {
			req.Constraint = declared
			req.Conflict = true
		}
	default:
		req.Constraint = inferred
		req.Source = strings.Join(sources, ", ")
	}

	if req.Preferred == "" {
		req.Preferred = choosePreferred(req.Constraint, hints, req.Legacy, lts)
	}
	if req.Source == "" {
		for _, h := range hints {
			if h.Prefer != "" && h.Prefer == req.Preferred {
				req.Source = h.Source
				break
			}
		}
	}
	if req.Source == "" && req.Constraint == "" {
		req.Source = "default"
	}
	return req
}

// hasSourcePkg 判断依据列表里是否已有同一个包（"vite 6.4" 与 "vite" 视为同一个）。
func hasSourcePkg(sources []string, src string) bool {
	name := strings.Fields(src)[0]
	for _, s := range sources {
		if strings.Fields(s)[0] == name {
			return true
		}
	}
	return false
}

// choosePreferred：webpack 4 一类老工程偏好规则给出的版本（通常 16）；其余取满足约束的最新 LTS。
func choosePreferred(constraint string, hints []NodeHint, legacy bool, lts LTSInfo) string {
	for _, h := range hints {
		if h.Prefer == "" {
			continue
		}
		if n, err := strconv.Atoi(h.Prefer); err == nil && AllowsMajor(constraint, n) {
			return h.Prefer
		}
	}
	for _, m := range lts.Majors {
		if AllowsMajor(constraint, m) {
			return strconv.Itoa(m)
		}
	}
	return ""
}

// majorMinor 取 "6.0.7" → "6.0"，预发布版本只取主版本。
func majorMinor(v string) string {
	pv, ok := sdk.ParseVersion(v)
	if !ok {
		return ""
	}
	if pv.N >= 2 {
		return fmt.Sprintf("%d.%d", pv.Parts[0], pv.Parts[1])
	}
	return strconv.Itoa(pv.Parts[0])
}
