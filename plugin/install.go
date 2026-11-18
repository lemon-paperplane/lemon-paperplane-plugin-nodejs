package nodejs

import (
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// EnvRelaxedLockfile 设为 false 时安装不再要求锁文件完全一致（仍然不写锁文件），由「锁文件与 package.json 不一致」的修复设置。
const EnvRelaxedLockfile = "PAPERPLANE_FROZEN_LOCKFILE"

// InstallArgs 生成不改锁文件的安装参数。relaxed：锁文件与 package.json 不一致时，按锁文件尽量安装但不写回。
func InstallArgs(pm PMInfo, hasLock, relaxed bool) []string {
	switch pm.Name {
	case PMYarn:
		if pm.Berry {
			if hasLock {
				return []string{"install", "--immutable"}
			}
			return []string{"install"}
		}
		// yarn 1 直接下载锁文件里 resolved 的地址（不走镜像），慢网络下默认 30 秒超时太短
		if hasLock && !relaxed {
			return []string{"install", "--frozen-lockfile", "--non-interactive", "--network-timeout", "300000"}
		}
		return []string{"install", "--pure-lockfile", "--non-interactive", "--network-timeout", "300000"}
	case PMPnpm:
		if hasLock && !relaxed {
			return []string{"install", "--frozen-lockfile"}
		}
		return []string{"install", "--no-lockfile"}
	case PMBun:
		if hasLock && !relaxed {
			return []string{"install", "--frozen-lockfile"}
		}
		return []string{"install", "--no-save"}
	default:
		switch {
		case hasLock && !relaxed:
			return []string{"ci", "--no-audit", "--no-fund"}
		case hasLock:
			return []string{"install", "--no-save", "--no-audit", "--no-fund"}
		}
		return []string{"install", "--no-package-lock", "--no-audit", "--no-fund"}
	}
}

// hasUsableLock 判断选定的锁文件是否存在（npm ci / --frozen-lockfile 需要它）。
func hasUsableLock(dir string, pm PMInfo) bool {
	return pm.Lockfile != "" && sdk.FileExists(filepath.Join(dir, pm.Lockfile))
}

// binaryMirrorEnv 是常见「安装时另外下载二进制」的包的国内镜像（只在国内区域、且用户没自己设置时生效）。
func binaryMirrorEnv(m sdk.Mirrors) map[string]string {
	if m.Region != "china" {
		return nil
	}
	base := "https://cdn.npmmirror.com/binaries"
	if strings.HasSuffix(strings.TrimSuffix(m.NodeDist, "/"), "/binaries/node") {
		base = strings.TrimSuffix(strings.TrimSuffix(m.NodeDist, "/"), "/node")
	}
	return map[string]string{
		"SASS_BINARY_SITE":                     base + "/node-sass",
		"ELECTRON_MIRROR":                      base + "/electron/",
		"ELECTRON_BUILDER_BINARIES_MIRROR":     base + "/electron-builder-binaries/",
		"PUPPETEER_DOWNLOAD_BASE_URL":          base + "/chrome-for-testing",
		"PLAYWRIGHT_DOWNLOAD_HOST":             base + "/playwright",
		"CHROMEDRIVER_CDNURL":                  base + "/chromedriver",
		"SENTRYCLI_CDNURL":                     base + "/sentry-cli",
		"npm_config_sharp_binary_host":         base + "/sharp",
		"npm_config_sharp_libvips_binary_host": base + "/sharp-libvips",
		"npm_config_canvas_binary_host_mirror": base + "/canvas",
		"npm_config_disturl":                   strings.TrimSuffix(firstNonEmpty(m.NodeDist, base+"/node"), "/"),
	}
}

// installEnv 组装安装时的环境：项目环境 + 工具链 + 二进制镜像 + 安静选项。
func installEnv(ctx *sdk.Context, base map[string]string, node *sdk.Toolchain, tool *pmTool) map[string]string {
	env := sdk.MergeEnv(base)
	if node != nil {
		for k, v := range node.Env {
			env[k] = v
		}
	}
	if ctx.Init != nil && sdk.OptBool(ctx.Init.Settings, "binaryMirrors", true) {
		for k, v := range binaryMirrorEnv(ctx.Init.Mirrors) {
			if _, set := env[k]; !set && os.Getenv(k) == "" {
				env[k] = v
			}
		}
	}
	for k, v := range map[string]string{
		"HUSKY":                      "0", // 不让 husky 在安装时改 .git/config
		"npm_config_fund":            "false",
		"npm_config_audit":           "false",
		"npm_config_update_notifier": "false",
		"ADBLOCK":                    "1",
		"DISABLE_OPENCOLLECTIVE":     "1",
		"YARN_ENABLE_TELEMETRY":      "0",
	} {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	for k, v := range tool.Env {
		env[k] = v
	}
	if v, ok := sanitizeNodeOptions(env, node); ok {
		env["NODE_OPTIONS"] = v
	}
	return env
}

// InstallDependencies 安装依赖，输出进任务控制台。
func (pl *Plugin) InstallDependencies(ctx *sdk.Context, p *sdk.InstallDepsParams) (*sdk.InstallDepsResult, error) {
	var node *sdk.Toolchain
	if tc, ok := p.Toolchains["node"]; ok {
		node = &tc
	}
	st, err := pl.install(ctx, p.Module, node, p.Env, p.TaskID, p.Clean)
	if err != nil {
		return nil, err
	}
	return &sdk.InstallDepsResult{Dependencies: *st}, nil
}

func (pl *Plugin) install(ctx *sdk.Context, mod sdk.ModuleInfo, node *sdk.Toolchain, baseEnv map[string]string, console string, clean bool) (*sdk.DependencyState, error) {
	dir := mod.Dir
	pkg, err := ReadPackageJSON(dir)
	if err != nil {
		return nil, sdk.UserError("%s", ctx.T("run.noPackageJson", dir))
	}
	pm := DetectPM(dir, pkg)
	tool, err := pl.ResolvePM(ctx, pm, node, dir, console)
	if err != nil {
		return nil, err
	}
	if clean {
		pl.clean(ctx, dir, pkg, console)
	}
	relaxed := strings.EqualFold(baseEnv[EnvRelaxedLockfile], "false")
	args := append(append([]string{}, tool.Prefix...), InstallArgs(pm, hasUsableLock(dir, pm), relaxed)...)
	var pp []string
	pp = append(pp, tool.PathPrepend...)
	if b := nodeBinDir(node); b != "" {
		pp = append(pp, b)
	}
	if pm.Lockfile != "" && len(pm.Lockfiles) > 1 {
		ctx.Host.Console(console, ctx.T("install.usingLockfile", pm.Lockfile, strings.Join(pm.Lockfiles, ", ")))
	}
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{
		Command: tool.Command, Args: args, Cwd: dir,
		Env: installEnv(ctx, baseEnv, node, tool), PathPrepend: pp,
		Console: console, Title: ctx.T("install.title"),
	})
	if err != nil {
		return nil, err
	}
	if r.Cancelled {
		return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if r.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("install.failed", pm.Name, r.ExitCode))
	}
	deps := CollectDeps(dir, pkg, pm)
	s := CheckDeps(dir, pkg, pm, deps)
	state := &sdk.DependencyState{State: s.State, Manager: pm.Name}
	if s.State == DepsOK {
		state.Message = ctx.T("deps.ok", pm.Display())
	} else if len(s.Missing) > 0 {
		state.Message = ctx.T("deps.missingSome", len(s.Missing), joinLimit(s.Missing, 3))
	}
	return state, nil
}

// clean 删除 node_modules（含 workspace 成员的）。只删包管理器的标准产物。
func (pl *Plugin) clean(ctx *sdk.Context, dir string, pkg *PackageJSON, console string) {
	targets := []string{filepath.Join(dir, "node_modules")}
	for _, m := range ExpandWorkspaces(dir, WorkspacePatterns(dir, pkg)) {
		targets = append(targets, filepath.Join(dir, filepath.FromSlash(m), "node_modules"))
	}
	for _, t := range targets {
		if !sdk.DirExists(t) {
			continue
		}
		ctx.Host.Console(console, ctx.T("install.removing", t))
		if err := os.RemoveAll(t); err != nil {
			ctx.Host.Console(console, err.Error())
		}
	}
}

// 动作 id。
const (
	ActionInstall   = "install"
	ActionReinstall = "reinstall"
)

// InvokeAction 处理「安装依赖」「清理重装」。
func (pl *Plugin) InvokeAction(ctx *sdk.Context, p *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	switch p.Action {
	case ActionInstall, ActionReinstall:
		var node *sdk.Toolchain
		if tc, ok := p.Toolchains["node"]; ok {
			node = &tc
		}
		st, err := pl.install(ctx, p.Module, node, p.Env, p.TaskID, p.Action == ActionReinstall)
		if err != nil {
			return nil, err
		}
		return &sdk.InvokeActionResult{Message: st.Message, Reimport: true}, nil
	}
	return nil, sdk.ErrNotSupported
}
