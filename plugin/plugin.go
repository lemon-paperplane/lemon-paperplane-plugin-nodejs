// Package nodejs 是 Lemon Paperplane 的 Node.js 插件：Node 工具链发现与下载、包管理器识别、
// package.json 脚本运行配置、依赖安装与状态、Node 版本推断，以及常见报错的诊断规则。
package nodejs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// PluginID 是插件 id。
const PluginID = "nodejs"

// Plugin 实现 nodejs 插件。插件无状态，所有信息每次从文件读取。
type Plugin struct{}

// New 返回插件实例（多入口二进制按此约定构造）。
func New() any { return &Plugin{} }

// Model 是 analyze 产出、内核原样保存并传给依赖本插件的框架插件的模型。
type Model struct {
	Name           string            `json:"name,omitempty"`
	PackageManager PMInfo            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts,omitempty"`
	ScriptOrder    []string          `json:"scriptOrder,omitempty"`
	DefaultScript  string            `json:"defaultScript,omitempty"`
	Versions       map[string]string `json:"versions,omitempty"` // 直接依赖实际版本：已安装 > 锁文件 > 声明下限
	Workspaces     []string          `json:"workspaces,omitempty"`
	Node           NodeRequirement   `json:"node"`
	LegacyPackage  string            `json:"legacyPackage,omitempty"` // 触发 OpenSSL 旧版兼容需求的依赖名
	Dependencies   string            `json:"dependencies,omitempty"`  // 依赖状态
}

// Initialize：发行索引超过一天没刷新就在后台刷新一次（LTS 判断用），不阻塞初始化。
func (pl *Plugin) Initialize(ctx *sdk.Context) error {
	if ctx.Init == nil || ctx.Init.CacheDir == "" {
		return nil
	}
	if _, at := LoadCachedIndex(ctx.Init.CacheDir); time.Since(at) > 24*time.Hour {
		m, dir := ctx.Init.Mirrors, ctx.Init.CacheDir
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = FetchIndex(c, m, dir, true)
		}()
	}
	return nil
}

// skipSegments：这些目录里的 package.json 是第三方包或静态资源，不是工程。
var skipSegments = map[string]bool{
	"node_modules": true, "uni_modules": true, "bower_components": true, "jspm_packages": true,
	"unpackage": true, "static": true, "public": true, "vendor": true, "third_party": true, "wwwroot": true,
}

func skippedRelDir(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if skipSegments[seg] {
			return true
		}
	}
	return false
}

// Detect 识别含 package.json 的目录，认领 workspace 成员。
func (pl *Plugin) Detect(ctx *sdk.Context, p *sdk.DetectParams) (*sdk.DetectResult, error) {
	if !sdk.HasFile(p.Files, "package.json") || skippedRelDir(p.RelDir) {
		return &sdk.DetectResult{}, nil
	}
	// Unity 包：package.json 旁边有 Unity 生成的 package.json.meta（Assets 里嵌入的包、Library/PackageCache 里的包都有），
	// 是 Unity 包管理器的清单，不是 npm 工程
	if sdk.HasFile(p.Files, "package.json.meta") {
		return &sdk.DetectResult{}, nil
	}
	pkg, err := ReadPackageJSON(p.Dir)
	if err != nil {
		// 损坏的 package.json 仍算 Node 模块，analyze 里报问题
		return &sdk.DetectResult{Facets: []sdk.Facet{{ID: "nodejs"}}, Name: filepath.Base(p.Dir)}, nil
	}
	if pkg.UniModules || pkg.UnityPackage {
		return &sdk.DetectResult{}, nil
	}
	res := &sdk.DetectResult{
		Facets: []sdk.Facet{{ID: "nodejs"}},
		Name:   pkg.DisplayName(p.Dir),
		Claims: ExpandWorkspaces(p.Dir, WorkspacePatterns(p.Dir, pkg)),
	}
	return res, nil
}

// Analyze 静态分析：包管理器、Node 需求、运行配置、依赖状态。
func (pl *Plugin) Analyze(ctx *sdk.Context, p *sdk.AnalyzeParams) (*sdk.AnalyzeResult, error) {
	dir := p.Module.Dir
	pkg, err := ReadPackageJSON(dir)
	if err != nil {
		return &sdk.AnalyzeResult{
			Dependencies: &sdk.DependencyState{State: DepsUnknown},
			Problems: []sdk.Diagnosis{{
				Severity: "error", Title: ctx.T("problem.packageJson.title"), Message: err.Error(),
			}},
		}, nil
	}
	a := pl.analyze(ctx, p, pkg)
	return a.result(ctx, p), nil
}

// analysis 是一次分析的中间结果，analyze / 视图 / 工具共用。
type analysis struct {
	dir     string
	pkg     *PackageJSON
	pm      PMInfo
	deps    []DepInfo
	status  DepStatus
	req     NodeRequirement
	members []string
	hints   scriptHints
	configs []sdk.RunConfiguration
	def     string
	model   Model
}

func (pl *Plugin) analyze(ctx *sdk.Context, p *sdk.AnalyzeParams, pkg *PackageJSON) *analysis {
	dir := p.Module.Dir
	a := &analysis{dir: dir, pkg: pkg}
	a.pm = DetectPM(dir, pkg)
	a.deps = CollectDeps(dir, pkg, a.pm)
	versions := map[string]string{}
	engines := map[string]string{}
	for _, d := range a.deps {
		if v := d.Version(); v != "" {
			versions[d.Name] = v
		}
		if d.Engine != "" {
			engines[d.Name] = d.Engine
		}
	}
	lts := LoadLTS(cacheDir(ctx))
	pin := FindVersionPin(dir, p.Project.Root, pkg)
	a.req = ResolveNodeRequirement(pkg, pin, versions, engines, lts)
	a.status = CheckDeps(dir, pkg, a.pm, a.deps)
	a.members = ExpandWorkspaces(dir, WorkspacePatterns(dir, pkg))
	a.hints = facetHints(p.Module.Facets)
	a.configs, a.def = buildRunConfigs(ctx, p.Module, pkg, a.members, a.hints)
	a.model = Model{
		Name:           pkg.Name,
		PackageManager: a.pm,
		Scripts:        pkg.Scripts,
		ScriptOrder:    pkg.ScriptOrder,
		DefaultScript:  a.def,
		Versions:       versions,
		Workspaces:     a.members,
		Node:           a.req,
		Dependencies:   a.status.State,
	}
	a.model.Node.Hints = nil
	if a.req.Legacy {
		a.model.LegacyPackage = strings.Fields(a.req.LegacySource)[0]
	}
	return a
}

func (a *analysis) result(ctx *sdk.Context, p *sdk.AnalyzeParams) *sdk.AnalyzeResult {
	model, _ := json.Marshal(a.model)
	src := a.req.Source
	if src == "default" {
		src = ctx.T("source.default")
	}
	res := &sdk.AnalyzeResult{
		Model: model,
		Requirements: []sdk.ToolchainRequirement{{
			Kind: "node", Constraint: a.req.Constraint, Preferred: a.req.Preferred, Source: src,
		}},
		RunConfigurations: a.configs,
		Dependencies:      a.depState(ctx),
		Info:              a.info(ctx, p),
		Problems:          a.problems(ctx, p),
		OptionValues:      a.optionValues(ctx),
	}
	return res
}

func (a *analysis) depState(ctx *sdk.Context) *sdk.DependencyState {
	ds := &sdk.DependencyState{State: a.status.State, Manager: a.pm.Name}
	switch a.status.State {
	case DepsMissing:
		if n := len(a.status.Missing); n > 0 {
			ds.Message = ctx.T("deps.missingSome", n, joinLimit(a.status.Missing, 3))
		} else {
			ds.Message = ctx.T("deps.missing")
		}
	case DepsOutdated:
		if a.status.Reason == "packageJson" {
			ds.Message = ctx.T("deps.outdatedPackageJson")
		} else {
			ds.Message = ctx.T("deps.outdatedLockfile", a.pm.Lockfile)
		}
	case DepsOK:
		ds.Message = ctx.T("deps.ok", a.pm.Display())
	case DepsNone:
		ds.Message = ctx.T("deps.none")
	}
	return ds
}

func (a *analysis) info(ctx *sdk.Context, p *sdk.AnalyzeParams) []sdk.InfoItem {
	var items []sdk.InfoItem
	nodeVal := ""
	if tc, ok := p.Toolchains["node"]; ok && tc.Version != "" {
		nodeVal = tc.Version
	} else if a.req.Pinned && a.req.Preferred != "" {
		nodeVal = a.req.Preferred
	} else if a.req.Constraint != "" {
		nodeVal = shortConstraint(a.req.Constraint)
	} else if a.req.Preferred != "" && a.req.Source != "default" {
		nodeVal = a.req.Preferred
	} else {
		nodeVal = "LTS"
	}
	items = append(items, sdk.InfoItem{Label: "Node.js", Value: nodeVal, Icon: "icons/nodejs.svg"})
	pmVal := ""
	switch {
	case a.pm.Version != "":
		pmVal = a.pm.Version
	case a.pm.Name == PMYarn && a.pm.Berry:
		pmVal = "Berry"
	case a.pm.Name != PMNpm:
		pmVal = a.pm.Major
	}
	items = append(items, sdk.InfoItem{Label: a.pm.Name, Value: pmVal, Icon: "icons/" + a.pm.Name + ".svg"})
	if len(a.pm.Lockfiles) > 1 && a.pm.Lockfile != "" {
		items = append(items, sdk.InfoItem{Label: ctx.T("info.lockfile"), Value: a.pm.Lockfile})
	}
	if len(a.members) > 0 {
		items = append(items, sdk.InfoItem{Label: ctx.T("info.workspaces"), Value: itoa(len(a.members))})
	}
	return items
}

// shortConstraint 把长约束缩成「≥最低版本」，便于卡片展示。
func shortConstraint(c string) string {
	if !strings.Contains(c, "||") && len(c) <= 12 {
		return c
	}
	pc := sdk.ParseConstraint(c)
	rs := evalRanges([]string{c}, func(v ver3) bool { return pc.Check(v.sdk()) })
	if len(rs) == 0 {
		return c
	}
	return ">=" + rs[0].lo.String()
}

func (a *analysis) problems(ctx *sdk.Context, p *sdk.AnalyzeParams) []sdk.Diagnosis {
	var out []sdk.Diagnosis
	if a.req.Conflict && a.req.Inferred != "" {
		d := sdk.Diagnosis{
			Severity: "warning",
			Title:    ctx.T("problem.nodeConflict.title"),
			Message:  ctx.T("problem.nodeConflict.message", a.req.Source, firstNonEmpty(a.req.Preferred, a.req.Constraint), a.req.Inferred),
			Fixes: []sdk.Fix{{
				ID: "useInferred", Title: ctx.T("fix.useNode", shortConstraint(a.req.Inferred)), Kind: sdk.FixSetToolchain,
				Params: map[string]string{"kind": "node", "constraint": a.req.Inferred}, Recommended: true,
			}},
		}
		out = append(out, d)
	}
	// webpack 4（没有框架插件接手时）在 Node 17+ 上需要 OpenSSL 旧版兼容
	if a.model.LegacyPackage == "webpack" && !legacyEnabled(p.Env) {
		if v, ok := PredictNode(ctx, p, a.req); ok && v.Major() >= 17 {
			out = append(out, LegacyProblem(ctx, a.req.LegacySource, v.String()))
		}
	}
	return out
}

// legacyEnabled 判断工程环境（或系统环境）的 NODE_OPTIONS 里是否已开启 OpenSSL 旧版兼容。
func legacyEnabled(env map[string]string) bool {
	v, ok := env["NODE_OPTIONS"]
	if !ok {
		v = os.Getenv("NODE_OPTIONS")
	}
	return strings.Contains(v, "--openssl-legacy-provider")
}

// LegacyProblem 生成「需要 OpenSSL 旧版兼容」问题及两个修复。
func LegacyProblem(ctx *sdk.Context, source, nodeVersion string) sdk.Diagnosis {
	return sdk.Diagnosis{
		RuleID:   "node.openssl-legacy.static",
		Severity: "warning",
		Title:    ctx.T("problem.legacy.title"),
		Message:  ctx.T("problem.legacy.message", source, nodeVersion),
		Fixes: []sdk.Fix{
			{ID: "legacy", Title: ctx.T("fix.opensslLegacy"), Kind: sdk.FixAppendEnv, Params: map[string]string{"key": "NODE_OPTIONS", "value": "--openssl-legacy-provider", "scope": "project"}, Recommended: true},
			{ID: "node16", Title: ctx.T("fix.useNode", "16"), Kind: sdk.FixSetToolchain, Params: map[string]string{"kind": "node", "constraint": "16"}},
		},
	}
}

// PredictNode 预测内核会为模块选哪个 Node：已选定的优先；否则按需求在已知工具链里选（偏好 → 满足约束的最高）。
func PredictNode(ctx *sdk.Context, p *sdk.AnalyzeParams, req NodeRequirement) (sdk.Version, bool) {
	if tc, ok := p.Toolchains["node"]; ok {
		return sdk.ParseVersion(tc.Version)
	}
	var tcs []sdk.Toolchain
	if ctx.Host != nil {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		tcs, _ = ctx.Host.Toolchains(c, "node")
		cancel()
	}
	var vs []string
	for _, t := range tcs {
		vs = append(vs, t.Version)
	}
	if i := sdk.BestVersion(vs, req.Constraint, req.Preferred); i >= 0 {
		return sdk.ParseVersion(vs[i])
	}
	if req.Preferred != "" {
		return sdk.ParseVersion(req.Preferred)
	}
	return sdk.Version{}, false
}

func (a *analysis) optionValues(ctx *sdk.Context) map[string][]sdk.Option {
	scripts := []sdk.Option{} // 没有选项也输出 []，不能是 null（界面按数组解析）
	for _, s := range a.pkg.ScriptOrder {
		if a.pkg.IsLifecycleScript(s) {
			continue
		}
		scripts = append(scripts, sdk.Option{Value: s, Label: s + "  " + truncate(a.pkg.Scripts[s], 60)})
	}
	pkgs := []sdk.Option{{Value: "", Label: ctx.T("opt.packageDir.root")}}
	for _, m := range a.members {
		pkgs = append(pkgs, sdk.Option{Value: m, Label: m})
	}
	return map[string][]sdk.Option{"scripts": scripts, "packages": pkgs}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func joinLimit(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + " …"
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
