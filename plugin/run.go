package nodejs

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// RunArgs 生成运行脚本的参数：npm 需要 "--" 才把参数传给脚本，yarn/pnpm/bun 直接跟在后面。
func RunArgs(pm string, script string, extra []string) []string {
	args := []string{"run", script}
	if len(extra) == 0 {
		return args
	}
	if pm == PMNpm {
		args = append(args, "--")
	}
	return append(args, extra...)
}

// PrepareRun 返回运行 npm 脚本的主进程描述。
func (pl *Plugin) PrepareRun(ctx *sdk.Context, p *sdk.PrepareRunParams) (*sdk.LaunchSpec, error) {
	if p.Config.Type != RunTypeNpmScript {
		return nil, sdk.ErrNotSupported
	}
	opts := p.Config.Options
	script := sdk.OptString(opts, "script", "")
	pkgDir := filepath.FromSlash(sdk.OptString(opts, "packageDir", ""))
	dir := filepath.Join(p.Module.Dir, pkgDir)
	pkg, err := ReadPackageJSON(dir)
	if err != nil {
		return nil, sdk.UserError("%s", ctx.T("run.noPackageJson", dir))
	}
	if script == "" {
		script = PickDefaultScript(pkg, nil, nil)
	}
	if _, ok := pkg.Scripts[script]; !ok {
		return nil, sdk.UserError("%s", ctx.T("run.noScript", script))
	}
	rootPkg := pkg
	if pkgDir != "" {
		if rp, err := ReadPackageJSON(p.Module.Dir); err == nil {
			rootPkg = rp
		}
	}
	pm := DetectPM(p.Module.Dir, rootPkg)
	if o := sdk.OptString(opts, "packageManager", ""); o != "" && o != "auto" && o != pm.Name {
		pm = PMInfo{Name: o, Source: "option"}
	}
	var node *sdk.Toolchain
	if tc, ok := p.Toolchains["node"]; ok {
		node = &tc
	} else if _, err := exec.LookPath("node"); err != nil {
		return nil, sdk.UserError("%s", ctx.T("run.noNode"))
	}
	tool, err := pl.ResolvePM(ctx, pm, node, dir, p.RunID)
	if err != nil {
		return nil, err
	}
	extra := sdk.SplitArgs(sdk.OptString(opts, "args", ""))
	args := append(append([]string{}, tool.Prefix...), RunArgs(pm.Name, script, extra)...)
	env := copyEnv(tool.Env)
	if v, ok := sanitizeNodeOptions(p.Env, node); ok {
		env["NODE_OPTIONS"] = v
		ctx.Host.Console(p.RunID, ctx.T("run.nodeOptionsSanitized", node.Version))
	}
	display := pm.Name + " " + strings.Join(RunArgs(pm.Name, script, extra), " ")
	// 默认直接运行脚本命令（像 npm 内部那样把 node_modules/.bin 放进 PATH），省掉常驻的包管理器进程（约 100 MB）
	if spec, ok := directRun(ctx, p, pkg, script, extra, dir, pm, tool, node, env); ok {
		return spec, nil
	}
	return &sdk.LaunchSpec{
		Command:     tool.Command,
		Args:        args,
		Cwd:         dir,
		Env:         env,
		PathPrepend: tool.PathPrepend,
		Display:     display,
	}, nil
}

// directRun 尝试不经包管理器直接运行脚本。以下情况回退到包管理器：
// 运行配置选项 runner=packageManager；Yarn PnP（依赖不在 node_modules 里）；脚本为空。
func directRun(ctx *sdk.Context, p *sdk.PrepareRunParams, pkg *PackageJSON, script string, extra []string, dir string, pm PMInfo, tool *pmTool, node *sdk.Toolchain, env map[string]string) (*sdk.LaunchSpec, bool) {
	if sdk.OptString(p.Config.Options, "runner", "direct") == "packageManager" {
		return nil, false
	}
	body := strings.TrimSpace(pkg.Scripts[script])
	if body == "" {
		return nil, false
	}
	for d := dir; ; d = filepath.Dir(d) {
		if sdk.FileExists(filepath.Join(d, ".pnp.cjs")) || sdk.FileExists(filepath.Join(d, ".pnp.js")) {
			return nil, false
		}
		if d == p.Module.Dir || d == filepath.Dir(d) || !strings.HasPrefix(d, p.Module.Dir) {
			break
		}
	}
	// 与 npm 一致：先 pre<script>，再脚本本身（附加参数），成功后 post<script>
	var parts []string
	if pre := strings.TrimSpace(pkg.Scripts["pre"+script]); pre != "" {
		parts = append(parts, pre)
	}
	main := body
	for _, a := range extra {
		main += " " + shellQuote(a)
	}
	parts = append(parts, main)
	if post := strings.TrimSpace(pkg.Scripts["post"+script]); post != "" {
		parts = append(parts, post)
	}
	cmdline := strings.Join(parts, " && ")
	// PATH：从脚本所在目录到工程根，各级 node_modules/.bin（workspace 依赖提升时 bin 在上层）
	var bins []string
	for d := dir; ; d = filepath.Dir(d) {
		if b := filepath.Join(d, "node_modules", ".bin"); sdk.DirExists(b) {
			bins = append(bins, b)
		}
		if d == p.Project.Root || d == filepath.Dir(d) || !strings.HasPrefix(d, p.Project.Root) {
			break
		}
	}
	path := append(bins, tool.PathPrepend...)
	// 包管理器在脚本运行时设置的常用变量（一些构建配置会读 npm_lifecycle_event 等）
	env["npm_lifecycle_event"] = script
	env["npm_lifecycle_script"] = body
	env["npm_package_name"] = pkg.Name
	env["npm_package_version"] = pkg.Version
	env["npm_package_json"] = filepath.Join(dir, "package.json")
	env["INIT_CWD"] = dir
	env["npm_config_user_agent"] = pm.Name + " node"
	if node != nil && node.Executable != "" {
		env["npm_node_execpath"] = node.Executable
		env["NODE"] = node.Executable
	}
	command, args := "/bin/sh", []string{"-c", cmdline}
	if runtime.GOOS == "windows" {
		command, args = "cmd", []string{"/d", "/s", "/c", cmdline}
	}
	ctx.Host.Console(p.RunID, ctx.T("run.direct", pm.Name))
	return &sdk.LaunchSpec{
		Command:     command,
		Args:        args,
		Cwd:         dir,
		Env:         env,
		PathPrepend: path,
		Display:     cmdline,
	}, true
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"$`\\|&;<>()*?[]{}!#~") {
		return s
	}
	if runtime.GOOS == "windows" {
		return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
