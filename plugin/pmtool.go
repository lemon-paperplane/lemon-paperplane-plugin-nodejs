package nodejs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// pmTool 是解析好的包管理器可执行方式。
type pmTool struct {
	Command     string            // 可执行文件
	Prefix      []string          // 固定前置参数（corepack 时为 "pnpm@9"）
	PathPrepend []string          // 需要加到 PATH 前面的目录（corepack 垫片、插件安装的包管理器）
	Env         map[string]string // 额外环境变量
	Via         string            // toolchain | path | corepack | managed
}

// corepackEnv：不弹下载确认、不往 package.json 写 packageManager、不因包管理器不一致报错。
var corepackEnv = map[string]string{
	"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
	"COREPACK_ENABLE_AUTO_PIN":        "0",
	"COREPACK_ENABLE_STRICT":          "0",
}

func nodeBinDir(node *sdk.Toolchain) string {
	if node == nil {
		return ""
	}
	if len(node.PathPrepend) > 0 {
		return node.PathPrepend[0]
	}
	if node.Executable != "" {
		return filepath.Dir(node.Executable)
	}
	_, bin := nodeExe(node.Path, runtime.GOOS)
	return bin
}

// findExe 在目录里找可执行文件（Windows 依次试 .cmd .exe .bat）。
func findExe(dir, name string) string {
	if dir == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		for _, ext := range []string{".cmd", ".exe", ".bat"} {
			if p := filepath.Join(dir, name+ext); sdk.FileExists(p) {
				return p
			}
		}
		return ""
	}
	p := filepath.Join(dir, name)
	if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
		return p
	}
	return ""
}

// managedPMDir 是插件自己安装包管理器的前缀目录。
func managedPMDir(ctx *sdk.Context, spec string) string {
	base := os.TempDir()
	if ctx.Init != nil && ctx.Init.DataDir != "" {
		base = ctx.Init.DataDir
	}
	safe := regexp.MustCompile(`[^A-Za-z0-9@._-]+`).ReplaceAllString(spec, "_")
	return filepath.Join(base, "pm", safe)
}

func prefixBin(prefix string) string {
	if runtime.GOOS == "windows" {
		return prefix
	}
	return filepath.Join(prefix, "bin")
}

// ResolvePM 找到可用的包管理器：npm 用工具链自带；yarn/pnpm/bun 依次尝试
// packageManager 精确版本走 corepack → 现成的可执行文件 → corepack → 用 npm 装到插件数据目录。
func (pl *Plugin) ResolvePM(ctx *sdk.Context, pm PMInfo, node *sdk.Toolchain, cwd, console string) (*pmTool, error) {
	bin := nodeBinDir(node)
	if pm.Name == PMNpm {
		if p := findExe(bin, "npm"); p != "" {
			return &pmTool{Command: p, Via: "toolchain"}, nil
		}
		if p, err := exec.LookPath("npm"); err == nil {
			return &pmTool{Command: p, Via: "path"}, nil
		}
		return nil, sdk.UserError("%s", ctx.T("pm.notFound", "npm"))
	}
	corepack := ""
	if pm.Name != PMBun {
		corepack = findExe(bin, "corepack")
	}
	if pm.Exact && corepack != "" {
		return pl.viaCorepack(ctx, pm, node, corepack, console)
	}
	if t := pl.findExistingPM(ctx, pm, node, cwd); t != nil {
		return t, nil
	}
	if corepack != "" {
		return pl.viaCorepack(ctx, pm, node, corepack, console)
	}
	return pl.viaManaged(ctx, pm, node, console)
}

// pmSearchDirs 列出可能装有全局 yarn/pnpm/bun 的目录。
func pmSearchDirs(ctx *sdk.Context, pm PMInfo, node *sdk.Toolchain) []string {
	home := sdk.HomeDir()
	dirs := []string{nodeBinDir(node), prefixBin(managedPMDir(ctx, pm.Spec()))}
	switch pm.Name {
	case PMYarn:
		dirs = append(dirs, filepath.Join(home, ".yarn", "bin"))
	case PMPnpm:
		dirs = append(dirs, os.Getenv("PNPM_HOME"), filepath.Join(home, "Library", "pnpm"), filepath.Join(home, ".local", "share", "pnpm"))
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			dirs = append(dirs, filepath.Join(la, "pnpm"))
		}
	case PMBun:
		if bi := os.Getenv("BUN_INSTALL"); bi != "" {
			dirs = append(dirs, filepath.Join(bi, "bin"))
		}
		dirs = append(dirs, filepath.Join(home, ".bun", "bin"))
	}
	// 其它 Node 安装里的全局包（yarn 1 / pnpm 是 JS 脚本，用当前 Node 运行即可）
	if pm.Name != PMBun {
		for _, tc := range DiscoverNode(ctx, nil, hostDataDir(ctx)) {
			dirs = append(dirs, nodeBinDir(&tc))
		}
	}
	if p, err := exec.LookPath(pm.Name); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	return dirs
}

func hostDataDir(ctx *sdk.Context) string {
	if ctx.Init != nil {
		return ctx.Init.HostDataDir
	}
	return ""
}

// findExistingPM 在已有安装里找版本合适的包管理器。
func (pl *Plugin) findExistingPM(ctx *sdk.Context, pm PMInfo, node *sdk.Toolchain, cwd string) *pmTool {
	seen := map[string]bool{}
	for _, d := range pmSearchDirs(ctx, pm, node) {
		p := findExe(d, pm.Name)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if !pl.pmVersionOK(ctx, pm, node, p, cwd) {
			continue
		}
		t := &pmTool{Command: p, Via: "path"}
		if d == prefixBin(managedPMDir(ctx, pm.Spec())) {
			t.Via = "managed"
			t.PathPrepend = []string{d}
		}
		return t
	}
	return nil
}

// pmVersionOK 检查现成包管理器的版本：yarn classic 工程必须是 yarn 1（berry 会改写锁文件）；
// berry 工程有 yarnPath 时 yarn 1 也能转交；pnpm 主版本要与锁文件格式匹配。
func (pl *Plugin) pmVersionOK(ctx *sdk.Context, pm PMInfo, node *sdk.Toolchain, exe, cwd string) bool {
	switch pm.Name {
	case PMYarn:
		if pm.Berry && pm.YarnPath != "" {
			return true
		}
	case PMPnpm:
		if pm.Major == "" && pm.Version == "" {
			return true
		}
	default:
		return true
	}
	v := pl.exeVersion(ctx, exe, node, cwd)
	if v == "" {
		return false
	}
	major := majorOf(v)
	switch pm.Name {
	case PMYarn:
		if pm.Berry {
			return major != "1"
		}
		return major == "1"
	case PMPnpm:
		want := pm.Major
		if pm.Version != "" {
			want = majorOf(pm.Version)
		}
		if want == "9" {
			return major == "9" || major == "10" || major == "11"
		}
		return major == want
	}
	return true
}

// exeVersion 运行 `<exe> --version`（在 PATH 前面放上当前 Node，避免用错 Node）。
func (pl *Plugin) exeVersion(ctx *sdk.Context, exe string, node *sdk.Toolchain, cwd string) string {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, exe, "--version")
	if cwd != "" && sdk.DirExists(cwd) {
		cmd.Dir = cwd
	}
	env := os.Environ()
	if bin := nodeBinDir(node); bin != "" {
		env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	for k, v := range corepackEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// viaCorepack 用 Node 自带的 corepack 运行指定版本；同时生成垫片目录放进 PATH，让脚本里嵌套的 yarn/pnpm 也能找到。
func (pl *Plugin) viaCorepack(ctx *sdk.Context, pm PMInfo, node *sdk.Toolchain, corepack, console string) (*pmTool, error) {
	t := &pmTool{Command: corepack, Prefix: []string{pm.Spec()}, Env: copyEnv(corepackEnv), Via: "corepack"}
	if ctx.Init != nil && ctx.Init.DataDir != "" && node != nil {
		shims := filepath.Join(ctx.Init.DataDir, "corepack", node.Version)
		if findExe(shims, pm.Name) == "" {
			_ = os.MkdirAll(shims, 0o755)
			r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{
				Command: corepack, Args: []string{"enable", "--install-directory", shims, pm.Name},
				Env: t.Env, PathPrepend: []string{nodeBinDir(node)}, Console: console, Title: ctx.T("pm.corepackEnable"), TimeoutMs: 60000,
			})
			if err != nil || r.ExitCode != 0 {
				// 垫片失败不影响主命令，只是嵌套调用找不到
				ctx.Host.Log("warn", "corepack enable failed: %v", err)
			}
		}
		if findExe(shims, pm.Name) != "" {
			t.PathPrepend = []string{shims}
		}
	}
	return t, nil
}

// viaManaged 用 npm 把包管理器装到插件数据目录（不动全局）。
func (pl *Plugin) viaManaged(ctx *sdk.Context, pm PMInfo, node *sdk.Toolchain, console string) (*pmTool, error) {
	spec := pm.Spec()
	pkgSpec := spec
	if pm.Name == PMYarn && pm.Berry {
		if pm.YarnPath != "" {
			pkgSpec = "yarn@1" // yarn 1 会转交给工程里的 yarnPath
		} else {
			v := pm.Version
			if v == "" {
				v = "4"
			}
			pkgSpec = "@yarnpkg/cli-dist@" + v
		}
	}
	prefix := managedPMDir(ctx, spec)
	bin := prefixBin(prefix)
	if findExe(bin, pm.Name) == "" {
		npm := findExe(nodeBinDir(node), "npm")
		if npm == "" {
			if p, err := exec.LookPath("npm"); err == nil {
				npm = p
			}
		}
		if npm == "" {
			return nil, sdk.UserError("%s", ctx.T("pm.notFound", pm.Name))
		}
		var pp []string
		if b := nodeBinDir(node); b != "" {
			pp = []string{b}
		}
		r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{
			Command: npm, Args: []string{"install", "-g", "--prefix", prefix, pkgSpec, "--no-audit", "--no-fund"},
			PathPrepend: pp, Console: console, Title: ctx.T("pm.installing", pkgSpec),
		})
		if err != nil {
			return nil, err
		}
		if r.ExitCode != 0 || findExe(bin, pm.Name) == "" {
			return nil, sdk.UserError("%s", ctx.T("pm.installFailed", pkgSpec))
		}
	}
	return &pmTool{Command: findExe(bin, pm.Name), PathPrepend: []string{bin}, Via: "managed"}, nil
}

// sanitizeNodeOptions：Node 17 以下不认识 --openssl-legacy-provider，NODE_OPTIONS 里带着会直接起不来，去掉它。
// 返回需要覆盖的值；不需要改时 ok=false。
func sanitizeNodeOptions(env map[string]string, node *sdk.Toolchain) (string, bool) {
	val, set := env["NODE_OPTIONS"]
	if !set {
		val = os.Getenv("NODE_OPTIONS")
	}
	if node == nil || nodeMajor(node) == 0 || nodeMajor(node) >= 17 || !strings.Contains(val, "--openssl-legacy-provider") {
		return "", false
	}
	var keep []string
	for _, f := range strings.Fields(val) {
		if f != "--openssl-legacy-provider" {
			keep = append(keep, f)
		}
	}
	return strings.Join(keep, " "), true
}
