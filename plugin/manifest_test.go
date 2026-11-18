package nodejs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

var (
	Toolchain16 = sdk.Toolchain{Kind: "node", Version: "16.17.1"}
	Toolchain22 = sdk.Toolchain{Kind: "node", Version: "22.20.0"}
)

func testCtx() *sdk.Context { return &sdk.Context{Context: context.Background()} }

func loadManifest(t *testing.T) *sdk.Manifest {
	t.Helper()
	m, err := sdk.LoadManifest("..")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func subst(tpl string, groups []string) string {
	for i := len(groups) - 1; i >= 1; i-- {
		tpl = strings.ReplaceAll(tpl, "{"+itoa(i)+"}", groups[i])
	}
	return tpl
}

// TestDiagnosticRules 用真实日志行检查每条规则：hint 必须出现在命中的行里，正则可编译，捕获组与修复参数正确。
func TestDiagnosticRules(t *testing.T) {
	m := loadManifest(t)
	rules := map[string]sdk.DiagnosticRule{}
	for _, r := range m.Contributes.Diagnostics {
		if _, err := regexp.Compile(r.Pattern); err != nil {
			t.Errorf("%s: %v", r.ID, err)
		}
		if r.Hint == "" {
			t.Errorf("%s: 缺少 hint", r.ID)
		}
		rules[r.ID] = r
	}
	cases := []struct {
		rule, line, fixParam, want string
	}{
		{"node.openssl-legacy", "Error: error:0308010C:digital envelope routines::unsupported", "", ""},
		{"node.port-in-use", "Error: listen EADDRINUSE: address already in use :::3000", "port", "3000"},
		{"node.port-in-use", "Error: listen EADDRINUSE: address already in use 127.0.0.1:5173", "port", "5173"},
		{"node.command-not-found", "sh: vite: command not found", "", ""},
		{"node.command-not-found", "sh: 1: vue-cli-service: not found", "", ""},
		{"node.command-not-found-zsh", "zsh:1: command not found: next", "", ""},
		{"node.command-not-recognized", "'vite' is not recognized as an internal or external command,", "", ""},
		{"node.command-not-recognized-zh", "'cross-env' 不是内部或外部命令，也不是可运行的程序", "", ""},
		{"node.module-not-found", "Error: Cannot find module 'webpack'", "", ""},
		{"node.package-not-found", "Error [ERR_MODULE_NOT_FOUND]: Cannot find package 'vite' imported from /x/vite.config.ts", "", ""},
		{"node.bin-permission", "sh: /app/node_modules/.bin/vite: Permission denied", "", ""},
		{"node.engine-yarn", `error vite@7.0.0: The engine "node" is incompatible with this module. Expected version "^20.19.0 || >=22.12.0". Got "18.17.1"`, "constraint", "^20.19.0 || >=22.12.0"},
		{"node.engine-npm", "npm WARN EBADENGINE   required: { node: '>=18' },", "constraint", ">=18"},
		{"node.engine-pnpm", "Expected version: >=18.12", "constraint", ">=18.12"},
		{"node.vite-requires-node", "You are using Node.js 18.17.1. Vite requires Node.js version 20.19+ or 22.12+. Please upgrade your Node.js version.", "constraint", "^20.19 || >=22.12"},
		{"node.vite-requires-node-single", "You are using Node.js 16.17.1. Vite requires Node.js version 18+. Please upgrade.", "constraint", ">=18"},
		{"node.crypto-hash", "TypeError: crypto.hash is not a function", "constraint", "^20.19 || >=22.12"},
		{"node.eresolve", "npm ERR! code ERESOLVE", "", ""},
		{"node.eresolve", "npm error ERESOLVE could not resolve", "", ""},
		{"node.lockfile-npm", "npm ERR! `npm ci` can only install packages when your package.json and package-lock.json or npm-shrinkwrap.json are in sync.", "", ""},
		{"node.lockfile-yarn", "error Your lockfile needs to be updated, but yarn was run with `--frozen-lockfile`.", "", ""},
		{"node.lockfile-pnpm", " ERR_PNPM_OUTDATED_LOCKFILE  Cannot install with \"frozen-lockfile\"", "", ""},
		{"node.heap-oom", "FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory", "value", "--max-old-space-size=4096"},
		{"node.gyp", "gyp ERR! stack Error: `make` failed with exit code: 2", "", ""},
		{"node.node-sass", "Error: Node Sass does not yet support your current environment: OS X Unsupported architecture (arm64)", "", ""},
		{"node.enospc-watchers", "Error: ENOSPC: System limit for number of file watchers reached, watch '/app/src'", "", ""},
		{"node.corepack-keyid", "Error: Cannot find matching keyid: {\"signatures\":[]}", "", ""},
		{"node.node-options-flag", "node: --openssl-legacy-provider is not allowed in NODE_OPTIONS", "", ""},
	}
	for _, c := range cases {
		r, ok := rules[c.rule]
		if !ok {
			t.Errorf("没有规则 %s", c.rule)
			continue
		}
		if !strings.Contains(c.line, r.Hint) {
			t.Errorf("%s: hint %q 不在行里", c.rule, r.Hint)
			continue
		}
		g := regexp.MustCompile(r.Pattern).FindStringSubmatch(c.line)
		if g == nil {
			t.Errorf("%s 没有命中：%s", c.rule, c.line)
			continue
		}
		if c.fixParam != "" {
			got := subst(r.Fixes[0].Params[c.fixParam], g)
			if got != c.want {
				t.Errorf("%s: 修复参数 %s = %q want %q", c.rule, c.fixParam, got, c.want)
			}
			if r.Fixes[0].Kind == sdk.FixSetToolchain && !Satisfiable(got) {
				t.Errorf("%s: 约束 %q 不可满足", c.rule, got)
			}
		}
	}
	// 不该误报
	negatives := map[string]string{
		"node.module-not-found":          "Error: Cannot find module './App.vue'",
		"node.command-not-found":         "[vite] page not found",
		"node.vite-requires-node-single": "Vite requires Node.js version 20.19+ or 22.12+.",
	}
	for id, line := range negatives {
		if regexp.MustCompile(rules[id].Pattern).MatchString(line) {
			t.Errorf("%s 误报：%s", id, line)
		}
	}
}

func TestURLAndReadyPatterns(t *testing.T) {
	m := loadManifest(t)
	lines := map[string]string{
		"  ➜  Local:   http://localhost:5173/":                               "http://localhost:5173/",
		"  - Local:   http://localhost:8080/":                                "http://localhost:8080/",
		"   - Local:        http://localhost:3000":                           "http://localhost:3000",
		"  Local:            http://localhost:3000":                          "http://localhost:3000",
		"<i> [webpack-dev-server] Loopback: http://localhost:8081/":          "http://localhost:8081/",
		"ready - started server on 0.0.0.0:3000, url: http://localhost:3000": "http://localhost:3000",
		"Server running at http://localhost:4000/":                           "http://localhost:4000/",
		"Example app listening on port 3001":                                 "http://localhost:3001",
	}
	for line, want := range lines {
		found := ""
		for _, u := range m.Contributes.URLPatterns {
			if u.Hint != "" && !strings.Contains(line, u.Hint) {
				continue
			}
			g := regexp.MustCompile(u.Pattern).FindStringSubmatch(line)
			if g == nil {
				continue
			}
			if u.URL != "" {
				found = subst(u.URL, g)
			} else {
				found = g[1]
			}
			break
		}
		if found != want {
			t.Errorf("%q → %q want %q", line, found, want)
		}
	}
	ready := []string{"  VITE v6.0.1  ready in 512 ms", "webpack 5.98.0 compiled successfully in 2301 ms", " DONE  Compiled successfully in 8231ms", " ✓ Ready in 1823ms", "DONE  Build complete. Watching for changes..."}
	for _, line := range ready {
		ok := false
		for _, r := range m.Contributes.ReadyPatterns {
			if strings.Contains(line, r.Hint) && regexp.MustCompile(r.Pattern).MatchString(line) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("就绪规则没有命中：%s", line)
		}
	}
}

// TestL10nCoverage：manifest 里的 %key% 与代码里 ctx.T 的 key 在两份文案里都要有。
func TestL10nCoverage(t *testing.T) {
	keys := map[string]bool{}
	b, _ := os.ReadFile("../plugin.json")
	for _, m := range regexp.MustCompile(`%([\w.\-:]+)%`).FindAllStringSubmatch(string(b), -1) {
		keys[m[1]] = true
	}
	files, _ := filepath.Glob("*.go")
	tre := regexp.MustCompile(`ctx\.T\("([^"]+)"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, m := range tre.FindAllStringSubmatch(string(src), -1) {
			if !strings.HasSuffix(m[1], ".") {
				keys[m[1]] = true
			}
		}
	}
	for _, loc := range []string{"zh-Hans", "en"} {
		var bundle map[string]string
		raw, err := os.ReadFile(filepath.Join("..", "l10n", loc+".json"))
		if err != nil || json.Unmarshal(raw, &bundle) != nil {
			t.Fatalf("l10n/%s.json: %v", loc, err)
		}
		for k := range keys {
			if bundle[k] == "" {
				t.Errorf("%s 缺少 %s", loc, k)
			}
		}
		for _, st := range []string{DepsOK, DepsMissing, DepsOutdated, DepsNone, DepsUnknown} {
			if bundle["deps.state."+st] == "" {
				t.Errorf("%s 缺少 deps.state.%s", loc, st)
			}
		}
	}
}

func TestNodeIndex(t *testing.T) {
	rs := []NodeRelease{
		{Version: "v27.0.0-alpha.1", Files: []string{"osx-arm64-tar"}},
		{Version: "v26.13.1", LTS: "Lithium", Files: []string{"osx-arm64-tar", "linux-x64", "win-x64-zip"}},
		{Version: "v25.9.0", Files: []string{"osx-arm64-tar", "linux-x64"}},
		{Version: "v24.22.3", LTS: "Krypton", Files: []string{"osx-arm64-tar", "linux-x64"}},
		{Version: "v22.20.0", LTS: "Jod", Files: []string{"osx-arm64-tar", "linux-x64"}},
		{Version: "v22.19.0", LTS: "Jod", Files: []string{"osx-arm64-tar", "linux-x64"}},
		{Version: "v16.20.2", LTS: "Gallium", Files: []string{"osx-arm64-tar", "osx-x64-tar", "linux-x64"}},
		{Version: "v14.21.3", LTS: "Fermium", Files: []string{"osx-x64-tar", "linux-x64"}},
	}
	lts := LTSFromIndex(rs)
	if lts.Latest() != 26 || lts.Codename["gallium"] != 16 {
		t.Errorf("lts %+v", lts)
	}
	av := AvailableVersions(rs, "darwin", "arm64")
	var vs []string
	for _, a := range av {
		vs = append(vs, a.Version)
		if a.Version == "26.13.1" && (!a.Recommended || !a.LTS) {
			t.Errorf("26 应为推荐 LTS")
		}
	}
	if strings.Join(vs, ",") != "26.13.1,24.22.3,22.20.0,16.20.2,14.21.3" {
		t.Errorf("available %v", vs)
	}
	for spec, want := range map[string]string{"22": "22.20.0", "lts/gallium": "16.20.2", "lts/*": "26.13.1", "^20.19 || >=22.12": "26.13.1", "22.19.0": "22.19.0", "node": "26.13.1"} {
		r, b, ok := ResolveRelease(rs, spec, "darwin", "arm64")
		if !ok || r.Ver() != want {
			t.Errorf("resolve %q = %v %v want %s", spec, r.Ver(), ok, want)
		}
		if ok && b.ext != "tar.gz" {
			t.Errorf("ext %s", b.ext)
		}
	}
	// 14 没有 arm64 构建，退回 x64（Rosetta）
	if _, b, ok := ResolveRelease(rs, "14", "darwin", "arm64"); !ok || b.file != "darwin-x64" {
		t.Errorf("14 on arm64: %+v %v", b, ok)
	}
	if shaFor("abc  node-v1.tar.gz\n"+strings.Repeat("a", 64)+"  node-v22.20.0-darwin-arm64.tar.gz\n", "node-v22.20.0-darwin-arm64.tar.gz") != strings.Repeat("a", 64) {
		t.Error("shaFor")
	}
}

func TestBuildRunConfigs(t *testing.T) {
	dir, _ := filepath.Abs(filepath.Join("testdata", "npm-ws"))
	pkg, _ := ReadPackageJSON(dir)
	mod := sdk.ModuleInfo{Dir: dir, Facets: []sdk.Facet{
		{ID: "nodejs"},
		{ID: "uniapp", Data: map[string]any{"preferScripts": []any{"build"}, "scriptTitles": map[string]any{"build": "构建"}, "hideScripts": []any{"dev"}}},
	}}
	h := facetHints(mod.Facets)
	cfgs, def := buildRunConfigs(testCtx(), mod, pkg, ExpandWorkspaces(dir, WorkspacePatterns(dir, pkg)), h)
	if def != "build" {
		t.Errorf("default %q", def)
	}
	var names []string
	for _, c := range cfgs {
		names = append(names, c.Name)
		if c.ID == "" || c.Env["BROWSER"] != "none" {
			t.Errorf("config %+v", c)
		}
	}
	if strings.Join(names, "|") != "构建|@ws/app: dev" {
		t.Errorf("configs %v", names)
	}
	again, _ := buildRunConfigs(testCtx(), mod, pkg, []string{"packages/app"}, h)
	if again[0].ID != cfgs[0].ID {
		t.Error("运行配置 id 必须稳定")
	}
}

func TestRenderDependenciesView(t *testing.T) {
	dir := fixture(t, "yarn1")
	pl := &Plugin{}
	res, err := pl.RenderView(testCtx(), &sdk.ViewRenderParams{View: ViewDependencies, Module: &sdk.ModuleInfo{Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	var table *sdk.ViewNode
	for i := range res.Root.Children {
		if res.Root.Children[i].Type == "table" {
			table = &res.Root.Children[i]
		}
	}
	if table == nil || len(table.Rows) != 3 || table.Rows[0].ID != "vue" || table.Rows[0].Cells["installed"] != "3.5.26" {
		t.Fatalf("table %+v", table)
	}
	out, err := pl.CallTool(testCtx(), &sdk.ToolCallParams{Tool: "scripts", Module: &sdk.ModuleInfo{Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	sr := out.(ScriptsResult)
	if sr.PackageManager != "yarn 1" || sr.DefaultScript != "dev" || len(sr.Scripts) != 3 || sr.Node.Constraint != "^20.19 || >=22.12" {
		t.Errorf("scripts tool %+v", sr)
	}
}

func TestDetectSkips(t *testing.T) {
	pl := &Plugin{}
	r, _ := pl.Detect(testCtx(), &sdk.DetectParams{Dir: filepath.Join("testdata", "yarn1"), RelDir: "uni_modules/uni-icons", Files: []string{"package.json"}})
	if len(r.Facets) != 0 {
		t.Error("uni_modules 里的包不是工程")
	}
	// Unity 包（旁边有 package.json.meta，或清单里有 "unity" 字段）不是 Node 工程
	r, _ = pl.Detect(testCtx(), &sdk.DetectParams{Dir: filepath.Join("testdata", "yarn1"), RelDir: "Assets/Plugins/UniTask", Files: []string{"package.json", "package.json.meta"}})
	if len(r.Facets) != 0 {
		t.Error("Unity 包（有 package.json.meta）不是工程")
	}
	upm := t.TempDir()
	_ = os.WriteFile(filepath.Join(upm, "package.json"), []byte(`{"name":"com.cysharp.unitask","version":"2.5.10","unity":"2018.4","dependencies":{}}`), 0o644)
	r, _ = pl.Detect(testCtx(), &sdk.DetectParams{Dir: upm, Files: []string{"package.json"}})
	if len(r.Facets) != 0 {
		t.Error("Unity 包清单（有 unity 字段）不是工程")
	}
	dir, _ := filepath.Abs(filepath.Join("testdata", "npm-ws"))
	r, _ = pl.Detect(testCtx(), &sdk.DetectParams{Dir: dir, Files: []string{"package.json", "packages/"}})
	if len(r.Facets) != 1 || r.Name != "npm-ws" || strings.Join(r.Claims, ",") != "packages/app" {
		t.Errorf("detect %+v", r)
	}
}

func TestApplyFixWithoutTask(t *testing.T) {
	r, err := (&Plugin{}).ApplyFix(testCtx(), &sdk.ApplyFixParams{Fix: sdk.Fix{ID: FixLegacyPeerDeps}})
	if err != nil || !r.Rerun || *r.Changes.Env[envLegacyPeerDeps] != "true" {
		t.Errorf("没有 TaskID 时交给内核重跑：%+v %v", r, err)
	}
	r, err = (&Plugin{}).ApplyFix(testCtx(), &sdk.ApplyFixParams{Fix: sdk.Fix{ID: FixRelaxedLockfile}})
	if err != nil || !r.Rerun || *r.Changes.Env[EnvRelaxedLockfile] != "false" {
		t.Errorf("relaxed: %+v %v", r, err)
	}
	if !legacyEnabled(map[string]string{"NODE_OPTIONS": "--openssl-legacy-provider"}) || legacyEnabled(map[string]string{"NODE_OPTIONS": ""}) {
		t.Error("legacyEnabled")
	}
}
