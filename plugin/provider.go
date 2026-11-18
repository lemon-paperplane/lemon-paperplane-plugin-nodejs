package nodejs

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"sort"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// ---------------------------------------------------------------------------
// 工具链

// DiscoverToolchains 发现本机 Node。
func (pl *Plugin) DiscoverToolchains(ctx *sdk.Context, p *sdk.DiscoverToolchainsParams) (*sdk.DiscoverToolchainsResult, error) {
	if p.Kind != "node" {
		return nil, sdk.ErrNotSupported
	}
	return &sdk.DiscoverToolchainsResult{Toolchains: DiscoverNode(ctx, p.Paths, hostDataDir(ctx))}, nil
}

func mirrors(ctx *sdk.Context) sdk.Mirrors {
	if ctx.Init == nil {
		return sdk.Mirrors{}
	}
	return ctx.Init.Mirrors
}

func cacheDir(ctx *sdk.Context) string {
	if ctx.Init == nil {
		return ""
	}
	return ctx.Init.CacheDir
}

func targetPlatform(ctx *sdk.Context) (string, string) {
	if ctx.Init != nil && ctx.Init.OS != "" && ctx.Init.Arch != "" {
		return ctx.Init.OS, ctx.Init.Arch
	}
	return runtime.GOOS, runtime.GOARCH
}

// AvailableToolchains 列出可下载的 Node（每个主版本最新一个，镜像优先）。
func (pl *Plugin) AvailableToolchains(ctx *sdk.Context, p *sdk.AvailableToolchainsParams) (*sdk.AvailableToolchainsResult, error) {
	if p.Kind != "node" {
		return nil, sdk.ErrNotSupported
	}
	rs, err := FetchIndex(ctx, mirrors(ctx), cacheDir(ctx), false)
	if err != nil {
		return nil, sdk.UserError("%s", ctx.T("toolchain.indexFailed", err.Error()))
	}
	goos, goarch := targetPlatform(ctx)
	return &sdk.AvailableToolchainsResult{Versions: AvailableVersions(rs, goos, goarch)}, nil
}

// InstallToolchain 返回下载计划，由内核下载解压。
func (pl *Plugin) InstallToolchain(ctx *sdk.Context, p *sdk.InstallToolchainParams) (*sdk.InstallToolchainResult, error) {
	if p.Kind != "node" {
		return nil, sdk.ErrNotSupported
	}
	rs, err := FetchIndex(ctx, mirrors(ctx), cacheDir(ctx), false)
	if err != nil {
		return nil, sdk.UserError("%s", ctx.T("toolchain.indexFailed", err.Error()))
	}
	goos, goarch := targetPlatform(ctx)
	r, b, ok := ResolveRelease(rs, p.Version, goos, goarch)
	if !ok {
		// 缓存可能过旧，强制刷新再试一次
		if fresh, err := FetchIndex(ctx, mirrors(ctx), cacheDir(ctx), true); err == nil {
			r, b, ok = ResolveRelease(fresh, p.Version, goos, goarch)
		}
	}
	if !ok {
		return nil, sdk.UserError("%s", ctx.T("toolchain.noVersion", p.Version))
	}
	return &sdk.InstallToolchainResult{Plan: InstallPlanFor(ctx, mirrors(ctx), r, b)}, nil
}

// ---------------------------------------------------------------------------
// 修复

// 自定义修复 id。
const (
	FixLegacyPeerDeps   = "legacyPeerDeps"
	FixRelaxedLockfile  = "relaxedLockfile"
	FixNodeSassVersion  = "nodeSassVersion"
	envLegacyPeerDeps   = "npm_config_legacy_peer_deps"
	envStrictPeerDeps   = "npm_config_strict_peer_dependencies"
	envRelaxedLockValue = "false"
)

// ApplyFix 处理 kind=custom 的修复，只改 Paperplane 保存的工程设置。内核给了 TaskID 时，
// 需要重装的修复直接在任务控制台里用新设置重装（输出实时可见），不再让内核另行重跑。
func (pl *Plugin) ApplyFix(ctx *sdk.Context, p *sdk.ApplyFixParams) (*sdk.ApplyFixResult, error) {
	switch p.Fix.ID {
	case FixLegacyPeerDeps:
		change := map[string]string{envLegacyPeerDeps: "true", envStrictPeerDeps: "false"}
		return pl.fixAndReinstall(ctx, p, change, ctx.T("fix.legacyPeerDeps.done"))
	case FixRelaxedLockfile:
		change := map[string]string{EnvRelaxedLockfile: envRelaxedLockValue}
		return pl.fixAndReinstall(ctx, p, change, ctx.T("fix.relaxedLockfile.done"))
	case FixNodeSassVersion:
		ver := ""
		if ip := readInstalled(p.Module.Dir, "node-sass"); ip != nil {
			ver = ip.Version
		} else if pkg, err := ReadPackageJSON(p.Module.Dir); err == nil {
			ver = cleanRange(pkg.DepRange("node-sass"))
		}
		for _, r := range nodeRules {
			if r.pkg == "node-sass" && ver != "" && sdk.MatchVersion(r.versions, ver) {
				return &sdk.ApplyFixResult{
					Changes: &sdk.ProjectChanges{Toolchains: map[string]string{"node": r.prefer}},
					Rerun:   true,
					Message: ctx.T("fix.useNode", r.prefer),
				}, nil
			}
		}
		return nil, sdk.UserError("%s", ctx.T("fix.nodeSass.unknown"))
	}
	return nil, sdk.ErrNotSupported
}

// fixAndReinstall 保存环境变量变更；有 TaskID 时立即带着新设置重装依赖。
func (pl *Plugin) fixAndReinstall(ctx *sdk.Context, p *sdk.ApplyFixParams, change map[string]string, msg string) (*sdk.ApplyFixResult, error) {
	env := map[string]*string{}
	for k, v := range change {
		v := v
		env[k] = &v
	}
	res := &sdk.ApplyFixResult{Changes: &sdk.ProjectChanges{Env: env}, Message: msg}
	if p.TaskID == "" {
		res.Rerun = true // 老内核：交给内核重跑安装
		return res, nil
	}
	var node *sdk.Toolchain
	if tc, ok := p.Toolchains["node"]; ok {
		node = &tc
	}
	st, err := pl.install(ctx, p.Module, node, sdk.MergeEnv(p.Env, change), p.TaskID, false)
	if err != nil {
		return nil, err
	}
	res.Message = ctx.T("fix.doneAndInstalled", msg, st.Message)
	return res, nil
}

// ---------------------------------------------------------------------------
// 依赖视图

// ViewDependencies 是「依赖」标签页的 id。
const ViewDependencies = "dependencies"

// RenderView 渲染依赖表。
func (pl *Plugin) RenderView(ctx *sdk.Context, p *sdk.ViewRenderParams) (*sdk.ViewRenderResult, error) {
	if p.View != ViewDependencies {
		return nil, sdk.ErrNotSupported
	}
	if p.Module == nil {
		return &sdk.ViewRenderResult{Root: sdk.Empty(ctx.T("view.deps.noModule"), "")}, nil
	}
	dir := p.Module.Dir
	pkg, err := ReadPackageJSON(dir)
	if err != nil {
		return &sdk.ViewRenderResult{Root: sdk.Alert("danger", ctx.T("problem.packageJson.title"), err.Error())}, nil
	}
	pm := DetectPM(dir, pkg)
	deps := CollectDeps(dir, pkg, pm)
	st := CheckDeps(dir, pkg, pm, deps)
	tone := map[string]string{DepsOK: "success", DepsMissing: "danger", DepsOutdated: "warning"}[st.State]
	if tone == "" {
		tone = "neutral"
	}
	header := sdk.Row(
		sdk.ViewNode{Type: "badge", Text: pm.Display(), Icon: "icons/" + pm.Name + ".svg"},
		sdk.Badge(ctx.T("deps.state."+st.State), tone),
	)
	if pm.Lockfile != "" {
		header.Children = append(header.Children, sdk.ViewNode{Type: "text", Text: pm.Lockfile, Mono: true, Tone: "neutral"})
	}
	header.Children = append(header.Children, sdk.ViewNode{Type: "spacer"}, sdk.ViewNode{Type: "button", Text: ctx.T("view.refresh"), Icon: "builtin:refresh-cw", Action: "refresh", Style: "secondary"})
	if len(deps) == 0 {
		return &sdk.ViewRenderResult{Root: sdk.Column(header, sdk.Empty(ctx.T("deps.none"), ""))}, nil
	}
	sort.SliceStable(deps, func(i, j int) bool {
		if deps[i].Dev != deps[j].Dev {
			return !deps[i].Dev
		}
		return deps[i].Name < deps[j].Name
	})
	cols := []sdk.TableColumn{
		{Key: "name", Title: ctx.T("view.deps.name"), Mono: true},
		{Key: "declared", Title: ctx.T("view.deps.declared"), Mono: true},
		{Key: "installed", Title: ctx.T("view.deps.installed"), Mono: true},
		{Key: "type", Title: ctx.T("view.deps.type"), Width: 90},
	}
	var rows []sdk.TableRow
	for _, d := range deps {
		typ := "prod"
		if d.Dev {
			typ = "dev"
		} else if d.Optional {
			typ = "optional"
		}
		row := sdk.TableRow{ID: d.Name, Cells: map[string]string{
			"name": d.Name, "declared": d.Declared, "installed": d.Installed, "type": ctx.T("view.deps.type." + typ),
		}}
		if d.Installed == "" && !d.Optional {
			row.Tone = "warning"
			row.Cells["installed"] = "—"
		}
		rows = append(rows, row)
	}
	return &sdk.ViewRenderResult{Root: sdk.Column(header, sdk.Table(cols, rows))}, nil
}

// HandleViewEvent 处理视图事件（只有刷新）。
func (pl *Plugin) HandleViewEvent(ctx *sdk.Context, p *sdk.ViewEventParams) (*sdk.ViewEventResult, error) {
	if p.View != ViewDependencies {
		return nil, sdk.ErrNotSupported
	}
	return &sdk.ViewEventResult{Refresh: true, State: p.State}, nil
}

// ---------------------------------------------------------------------------
// MCP 工具

// ScriptsResult 是 scripts 工具的返回。
type ScriptsResult struct {
	PackageManager string          `json:"packageManager"`
	Lockfile       string          `json:"lockfile,omitempty"`
	Dependencies   string          `json:"dependencies"`
	Node           NodeRequirement `json:"node"`
	DefaultScript  string          `json:"defaultScript,omitempty"`
	Scripts        []ScriptEntry   `json:"scripts"`
	Workspaces     []string        `json:"workspaces,omitempty"`
}

// ScriptEntry 是一个脚本。
type ScriptEntry struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Lifecycle bool   `json:"lifecycle,omitempty"`
	RunWith   string `json:"runWith"`
}

// CallTool 执行 scripts 工具。
func (pl *Plugin) CallTool(ctx *sdk.Context, p *sdk.ToolCallParams) (any, error) {
	if p.Tool != "scripts" {
		return nil, sdk.ErrNotSupported
	}
	if p.Module == nil {
		return nil, sdk.UserError("moduleId is required")
	}
	var args struct {
		PackageDir string `json:"packageDir"`
	}
	if len(p.Args) > 0 {
		_ = json.Unmarshal(p.Args, &args)
	}
	dir := filepath.Join(p.Module.Dir, filepath.FromSlash(args.PackageDir))
	pkg, err := ReadPackageJSON(dir)
	if err != nil {
		return nil, sdk.UserError("%s", err.Error())
	}
	pm := DetectPM(p.Module.Dir, pkg)
	deps := CollectDeps(dir, pkg, pm)
	versions, engines := map[string]string{}, map[string]string{}
	for _, d := range deps {
		versions[d.Name] = d.Version()
		if d.Engine != "" {
			engines[d.Name] = d.Engine
		}
	}
	root := p.Module.Dir
	if p.Project != nil {
		root = p.Project.Root
	}
	req := ResolveNodeRequirement(pkg, FindVersionPin(dir, root, pkg), versions, engines, LoadLTS(cacheDir(ctx)))
	req.Hints = nil
	out := ScriptsResult{
		PackageManager: pm.Display(),
		Lockfile:       pm.Lockfile,
		Dependencies:   CheckDeps(dir, pkg, pm, deps).State,
		Node:           req,
		DefaultScript:  PickDefaultScript(pkg, facetHints(p.Module.Facets).prefer, nil),
		Workspaces:     ExpandWorkspaces(dir, WorkspacePatterns(dir, pkg)),
	}
	for _, s := range pkg.ScriptOrder {
		out.Scripts = append(out.Scripts, ScriptEntry{
			Name: s, Command: pkg.Scripts[s], Lifecycle: pkg.IsLifecycleScript(s),
			RunWith: pm.Name + " " + joinArgs(RunArgs(pm.Name, s, nil)),
		})
	}
	return out, nil
}

func joinArgs(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
