package nodejs

import (
	"context"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixture 把 testdata/<name> 复制到临时目录（_node_modules 改名为 node_modules，避免被全局 gitignore 吃掉）。
func fixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := filepath.Join(t.TempDir(), name)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		rel = strings.ReplaceAll(rel, "_node_modules", "node_modules")
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestIntersectConstraints(t *testing.T) {
	cases := []struct {
		in   []string
		want string
		ok   bool
	}{
		{[]string{"^20.19 || >=22.12"}, "^20.19 || >=22.12", true},
		{[]string{">=18", "^20.19.0 || >=22.12.0"}, "^20.19 || >=22.12", true},
		{[]string{"^18.0.0 || ^20.0.0 || >=22.0.0", "^18 || >=20"}, "18 || 20 || >=22", true},
		{[]string{"<15", ">=16"}, "", false},
		{[]string{">=14.18", "<17"}, ">=14.18 <17", true},
		{[]string{"16.x"}, "16", true},
		{[]string{"", "*"}, "", true},
		{[]string{"^14.18 || >=16", ">=14"}, "^14.18 || >=16", true},
		{[]string{"18.17.1"}, "18.17.1", true},
		{[]string{">=0.10.0"}, ">=0.10", true},
	}
	for _, c := range cases {
		got, ok := IntersectConstraints(c.in...)
		if got != c.want || ok != c.ok {
			t.Errorf("IntersectConstraints(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if !AllowsMajor("^20.19 || >=22.12", 20) || AllowsMajor("^20.19 || >=22.12", 21) || !AllowsMajor("", 16) {
		t.Error("AllowsMajor")
	}
}

func TestPinConstraint(t *testing.T) {
	lts := builtinLTS()
	cases := []struct{ raw, c, p string }{
		{"lts/gallium", "16", "16"},
		{"v18.17.1", "18", "18.17.1"},
		{"16", "16", "16"},
		{"lts/*", "", "26"},
		{"lts/-1", "24", "24"},
		{"node", "", ""},
		{"system", "", ""},
		{">=18", ">=18", ""},
		{"20.x", "20.x", ""},
	}
	for _, c := range cases {
		gc, gp := PinConstraint(c.raw, lts)
		if gc != c.c || gp != c.p {
			t.Errorf("PinConstraint(%q) = %q,%q want %q,%q", c.raw, gc, gp, c.c, c.p)
		}
	}
}

func TestResolveNodeRequirement(t *testing.T) {
	lts := builtinLTS()
	// vite 7 + engines.node
	pkg := &PackageJSON{DevDependencies: map[string]string{"vite": "^7.3.0"}, Engines: map[string]string{"node": "^20.19.0 || >=22.12.0"}}
	r := ResolveNodeRequirement(pkg, nil, map[string]string{"vite": "7.3.1"}, nil, lts)
	if r.Constraint != "^20.19 || >=22.12" || r.Preferred != "26" || !strings.HasPrefix(r.Source, "package.json engines") {
		t.Errorf("vite7: %+v", r)
	}
	// Vue CLI 4（webpack 4）：偏好 16，需要旧版兼容
	pkg = &PackageJSON{DevDependencies: map[string]string{"@vue/cli-service": "~4.5.0"}}
	r = ResolveNodeRequirement(pkg, nil, map[string]string{"@vue/cli-service": "4.5.19"}, map[string]string{"@vue/cli-service": ">=8"}, lts)
	if r.Preferred != "16" || r.Constraint != ">=8" || !r.Legacy || r.LegacySource != "@vue/cli-service 4.5" || r.Source != "@vue/cli-service 4.5" {
		t.Errorf("vue cli 4: %+v", r)
	}
	// node-sass 4 限制上界
	pkg = &PackageJSON{DevDependencies: map[string]string{"node-sass": "^4.14.1", "@vue/cli-service": "~4.5.0"}}
	r = ResolveNodeRequirement(pkg, nil, map[string]string{"node-sass": "4.14.1", "@vue/cli-service": "4.5.19"}, nil, lts)
	if r.Constraint != "<15" || r.Preferred != "14" {
		t.Errorf("node-sass: %+v", r)
	}
	// 冲突的依赖 engines 被丢弃
	pkg = &PackageJSON{DevDependencies: map[string]string{"vite": "^7", "old": "1"}}
	r = ResolveNodeRequirement(pkg, nil, map[string]string{"vite": "7.0.0"}, map[string]string{"old": "<=12"}, lts)
	if r.Constraint != "^20.19 || >=22.12" {
		t.Errorf("advisory conflict: %+v", r)
	}
	// .nvmrc 钉住 16 但 vite 7 需要 20.19+：保留钉住版本并标记冲突
	pkg = &PackageJSON{DevDependencies: map[string]string{"vite": "^7.0.0"}}
	r = ResolveNodeRequirement(pkg, &VersionPin{Raw: "16", Source: ".nvmrc"}, map[string]string{"vite": "7.0.0"}, nil, lts)
	if r.Constraint != "16" || !r.Conflict || r.Inferred != "^20.19 || >=22.12" || r.Source != ".nvmrc" {
		t.Errorf("pin conflict: %+v", r)
	}
	// 没有任何线索：最新 LTS
	r = ResolveNodeRequirement(&PackageJSON{}, nil, nil, nil, lts)
	if r.Constraint != "" || r.Preferred != "26" || r.Source != "default" {
		t.Errorf("default: %+v", r)
	}
}

func TestFindVersionPin(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("testdata", "nvmrc"))
	pin := FindVersionPin(filepath.Join(root, "sub"), root, nil)
	if pin == nil || pin.Raw != "lts/gallium" || pin.Source != ".nvmrc" {
		t.Fatalf("pin = %+v", pin)
	}
	if p := FindVersionPin(filepath.Join(root, "sub"), filepath.Join(root, "sub"), nil); p != nil {
		t.Errorf("不应越过工程根：%+v", p)
	}
	pkg := &PackageJSON{Volta: map[string]string{"node": "20.11.1"}}
	if p := FindVersionPin(root, root, pkg); p == nil || p.Source != "package.json volta" {
		t.Errorf("volta: %+v", p)
	}
}

func TestParsePackageJSON(t *testing.T) {
	pkg, err := ReadPackageJSON(filepath.Join("testdata", "yarn1"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pkg.ScriptOrder, []string{"serve", "dev", "build"}) {
		t.Errorf("script order %v（非字符串脚本应跳过）", pkg.ScriptOrder)
	}
	if pkg.DisplayName("/x/yoyolearn-mp") != "yoyolearn-mp" {
		t.Errorf("模板名应改用目录名")
	}
	p2, _ := ParsePackageJSON([]byte("\xef\xbb\xbf" + `{"name":"a","workspaces":{"packages":["apps/*"]},"scripts":{"pretest":"x","test":"y","postinstall":"z","prepare":"husky"}}`))
	if !reflect.DeepEqual(p2.Workspaces, []string{"apps/*"}) {
		t.Errorf("workspaces %v", p2.Workspaces)
	}
	if got := p2.RunnableScripts(); !reflect.DeepEqual(got, []string{"test"}) {
		t.Errorf("runnable %v", got)
	}
}

func TestExpandWorkspaces(t *testing.T) {
	dir := filepath.Join("testdata", "npm-ws")
	pkg, _ := ReadPackageJSON(dir)
	got := ExpandWorkspaces(dir, WorkspacePatterns(dir, pkg))
	if !reflect.DeepEqual(got, []string{"packages/app"}) {
		t.Errorf("npm workspaces = %v", got)
	}
	dir = filepath.Join("testdata", "pnpm")
	pkg, _ = ReadPackageJSON(dir)
	if pats := WorkspacePatterns(dir, pkg); !reflect.DeepEqual(pats, []string{"packages/*", "!**/test/**"}) {
		t.Errorf("pnpm patterns %v", pats)
	}
	if got := ExpandWorkspaces(dir, WorkspacePatterns(dir, pkg)); !reflect.DeepEqual(got, []string{"packages/web"}) {
		t.Errorf("pnpm workspaces = %v", got)
	}
}

func TestDetectPM(t *testing.T) {
	check := func(name string, want PMInfo) {
		t.Helper()
		dir := filepath.Join("testdata", name)
		pkg, _ := ReadPackageJSON(dir)
		got := DetectPM(dir, pkg)
		if got.Name != want.Name || got.Version != want.Version || got.Berry != want.Berry || got.Major != want.Major || got.Lockfile != want.Lockfile {
			t.Errorf("%s: got %+v want %+v", name, got, want)
		}
	}
	check("yarn1", PMInfo{Name: PMYarn, Major: "1", Lockfile: "yarn.lock"})
	check("berry", PMInfo{Name: PMYarn, Version: "4.5.1", Berry: true, Major: "4", Lockfile: "yarn.lock"})
	check("pnpm", PMInfo{Name: PMPnpm, Version: "9.15.0", Major: "9", Lockfile: "pnpm-lock.yaml"})
	check("pnpm5", PMInfo{Name: PMPnpm, Major: "7", Lockfile: "pnpm-lock.yaml"})
	check("npm-ws", PMInfo{Name: PMNpm, Lockfile: "package-lock.json"})

	// 多个锁文件：看 node_modules 里谁最近装过，而不是锁文件修改时间
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"a":"1"}}`), 0o644)
	now := time.Now()
	touch(t, filepath.Join(dir, "yarn.lock"), now)
	touch(t, filepath.Join(dir, "package-lock.json"), now.Add(-time.Hour))
	touch(t, filepath.Join(dir, "node_modules", ".package-lock.json"), now.Add(-time.Hour))
	touch(t, filepath.Join(dir, "node_modules", ".yarn-integrity"), now.Add(-30*24*time.Hour))
	pkg, _ := ReadPackageJSON(dir)
	if pm := DetectPM(dir, pkg); pm.Name != PMNpm || pm.Lockfile != "package-lock.json" || len(pm.Lockfiles) != 2 {
		t.Errorf("by marker: %+v", pm)
	}
	// 没有 node_modules 时取最近修改的锁文件
	os.RemoveAll(filepath.Join(dir, "node_modules"))
	if pm := DetectPM(dir, pkg); pm.Name != PMYarn {
		t.Errorf("by mtime: %+v", pm)
	}
}

func TestLockedVersions(t *testing.T) {
	read := func(name string) map[string]lockedVer {
		dir := filepath.Join("testdata", name)
		pkg, _ := ReadPackageJSON(dir)
		return lockedVersions(dir, DetectPM(dir, pkg), pkg)
	}
	npm := read("npm-ws")
	if npm["vite"].version != "6.0.7" || npm["vite"].engine != "^18.0.0 || ^20.0.0 || >=22.0.0" || npm["typescript"].version != "5.6.3" {
		t.Errorf("npm lock: %+v", npm)
	}
	if _, ok := npm["esbuild"]; ok {
		t.Errorf("只应保留直接依赖：%+v", npm)
	}
	y := read("yarn1")
	if y["vite"].version != "7.3.1" || y["vue"].version != "3.5.26" || y["@vitejs/plugin-vue"].version != "6.0.3" {
		t.Errorf("yarn1 lock: %+v", y)
	}
	if b := read("berry"); b["express"].version != "4.21.2" {
		t.Errorf("berry lock: %+v", b)
	}
	if p := read("pnpm"); p["next"].version != "15.1.0" || p["react"].version != "19.0.0" || p["left-pad"].version != "" {
		t.Errorf("pnpm9 lock: %+v", p)
	}
	if p := read("pnpm5"); p["vue"].version != "2.7.16" || p["@vue/cli-service"].version != "4.5.19" || p["node-sass"].version != "4.14.1" {
		t.Errorf("pnpm5 lock: %+v", p)
	}
}

func TestCheckDeps(t *testing.T) {
	dir := fixture(t, "yarn1")
	pkg, _ := ReadPackageJSON(dir)
	pm := DetectPM(dir, pkg)
	st := CheckDeps(dir, pkg, pm, CollectDeps(dir, pkg, pm))
	if st.State != DepsMissing || !reflect.DeepEqual(st.Missing, []string{"@vitejs/plugin-vue"}) {
		t.Errorf("missing one: %+v", st)
	}
	os.MkdirAll(filepath.Join(dir, "node_modules", "@vitejs", "plugin-vue"), 0o755)
	old := time.Now().Add(-time.Hour)
	touch(t, filepath.Join(dir, "yarn.lock"), old)
	if st := CheckDeps(dir, pkg, pm, CollectDeps(dir, pkg, pm)); st.State != DepsOK {
		t.Errorf("ok: %+v", st)
	}
	// package.json 新增依赖 → integrity 不一致 → outdated（目录已存在，不算缺失）
	pkg.Dependencies["pinia"] = "^2.0.0"
	os.MkdirAll(filepath.Join(dir, "node_modules", "pinia"), 0o755)
	if st := CheckDeps(dir, pkg, pm, CollectDeps(dir, pkg, pm)); st.State != DepsOutdated || st.Reason != "packageJson" {
		t.Errorf("outdated: %+v", st)
	}
	// 没有 node_modules
	os.RemoveAll(filepath.Join(dir, "node_modules"))
	if st := CheckDeps(dir, pkg, pm, CollectDeps(dir, pkg, pm)); st.State != DepsMissing {
		t.Errorf("no node_modules: %+v", st)
	}
	if st := CheckDeps(dir, &PackageJSON{Name: "x"}, pm, nil); st.State != DepsNone {
		t.Errorf("none: %+v", st)
	}
}

func TestPickDefaultScript(t *testing.T) {
	mk := func(order []string, cmds ...string) *PackageJSON {
		p := &PackageJSON{Scripts: map[string]string{}, ScriptOrder: order}
		for i, s := range order {
			p.Scripts[s] = cmds[i]
		}
		return p
	}
	cases := []struct {
		pkg    *PackageJSON
		prefer []string
		want   string
	}{
		{mk([]string{"serve", "build", "dev"}, "vite preview", "vite build", "vite --host"), nil, "dev"},
		{mk([]string{"serve", "build", "lint"}, "vue-cli-service serve", "vue-cli-service build", "vue-cli-service lint"), nil, "serve"},
		{mk([]string{"serve", "dev:h5", "build:h5"}, "npm run dev:h5", "cross-env UNI_PLATFORM=h5 vue-cli-service uni-serve", "uni build"), []string{"dev:h5"}, "dev:h5"},
		{mk([]string{"build", "typecheck"}, "npx webpack --mode production", "tsc --noEmit"), nil, "build"},
		{mk([]string{"start", "build", "test"}, "react-scripts start", "react-scripts build", "react-scripts test"), nil, "start"},
		{mk([]string{"dev:custom", "dev:h5", "dev:mp-weixin"}, "uni -p", "uni", "uni -p mp-weixin"), nil, "dev:h5"},
		{mk([]string{"start", "dev"}, "node dist/server.js", "nodemon src/server.ts"), nil, "dev"},
	}
	for i, c := range cases {
		if got := PickDefaultScript(c.pkg, c.prefer, nil); got != c.want {
			t.Errorf("#%d got %q want %q", i, got, c.want)
		}
	}
}

func TestInstallArgs(t *testing.T) {
	cases := []struct {
		pm            PMInfo
		lock, relaxed bool
		want          string
	}{
		{PMInfo{Name: PMNpm}, true, false, "ci --no-audit --no-fund"},
		{PMInfo{Name: PMNpm}, true, true, "install --no-save --no-audit --no-fund"},
		{PMInfo{Name: PMNpm}, false, false, "install --no-package-lock --no-audit --no-fund"},
		{PMInfo{Name: PMYarn}, true, false, "install --frozen-lockfile --non-interactive --network-timeout 300000"},
		{PMInfo{Name: PMYarn}, false, false, "install --pure-lockfile --non-interactive --network-timeout 300000"},
		{PMInfo{Name: PMYarn, Berry: true}, true, true, "install --immutable"},
		{PMInfo{Name: PMPnpm}, true, false, "install --frozen-lockfile"},
		{PMInfo{Name: PMPnpm}, false, false, "install --no-lockfile"},
		{PMInfo{Name: PMBun}, true, false, "install --frozen-lockfile"},
		{PMInfo{Name: PMBun}, false, false, "install --no-save"},
	}
	for _, c := range cases {
		if got := strings.Join(InstallArgs(c.pm, c.lock, c.relaxed), " "); got != c.want {
			t.Errorf("%+v lock=%v relaxed=%v: %q want %q", c.pm, c.lock, c.relaxed, got, c.want)
		}
	}
	if got := strings.Join(RunArgs(PMNpm, "dev", []string{"--port", "3000"}), " "); got != "run dev -- --port 3000" {
		t.Errorf("npm run args %q", got)
	}
	if got := strings.Join(RunArgs(PMYarn, "dev", []string{"--port", "3000"}), " "); got != "run dev --port 3000" {
		t.Errorf("yarn run args %q", got)
	}
}

func TestPMSpec(t *testing.T) {
	cases := map[string]PMInfo{
		"pnpm@9.15.0": {Name: PMPnpm, Version: "9.15.0", Major: "9"},
		"pnpm@8":      {Name: PMPnpm, Major: "8"},
		"yarn@1":      {Name: PMYarn, Major: "1"},
		"yarn@stable": {Name: PMYarn, Berry: true},
		"bun@latest":  {Name: PMBun},
	}
	for want, pm := range cases {
		if got := pm.Spec(); got != want {
			t.Errorf("%+v: %q want %q", pm, got, want)
		}
	}
}

func TestArchFromHeader(t *testing.T) {
	macho := make([]byte, 16)
	binary.LittleEndian.PutUint32(macho, 0xfeedfacf)
	binary.LittleEndian.PutUint32(macho[4:], 0x0100000c)
	if a := archFromHeader(macho, nil); a != "arm64" {
		t.Errorf("macho %q", a)
	}
	elf := make([]byte, 64)
	copy(elf, "\x7fELF")
	elf[5] = 1
	binary.LittleEndian.PutUint16(elf[18:], 62)
	if a := archFromHeader(elf, nil); a != "amd64" {
		t.Errorf("elf %q", a)
	}
	pe := make([]byte, 256)
	pe[0], pe[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x80)
	copy(pe[0x80:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(pe[0x84:], 0xaa64)
	if a := archFromHeader(pe, nil); a != "arm64" {
		t.Errorf("pe %q", a)
	}
}

func TestSanitizeNodeOptions(t *testing.T) {
	n16 := &Toolchain16
	if v, ok := sanitizeNodeOptions(map[string]string{"NODE_OPTIONS": "--openssl-legacy-provider --max-old-space-size=4096"}, n16); !ok || v != "--max-old-space-size=4096" {
		t.Errorf("node16: %q %v", v, ok)
	}
	n22 := &Toolchain22
	if _, ok := sanitizeNodeOptions(map[string]string{"NODE_OPTIONS": "--openssl-legacy-provider"}, n22); ok {
		t.Errorf("node22 不应改")
	}
}

func TestShortConstraint(t *testing.T) {
	cases := map[string]string{"^20.19 || >=22.12": ">=20.19", ">=18": ">=18", "16": "16", "18 || 20 || >=22": ">=18"}
	for in, want := range cases {
		if got := shortConstraint(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestDiscoverPaths(t *testing.T) {
	host := t.TempDir()
	root := filepath.Join(host, "toolchains", "node", "20.11.1")
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.MkdirAll(filepath.Join(root, "include", "node"), 0o755)
	os.WriteFile(filepath.Join(root, "bin", "node"), []byte("#!/bin/sh\necho v0.0.0\n"), 0o755)
	os.WriteFile(filepath.Join(root, "node.exe"), []byte("MZ"), 0o755)
	os.WriteFile(filepath.Join(root, "include", "node", "node_version.h"), []byte("#define NODE_MAJOR_VERSION 20\n#define NODE_MINOR_VERSION 11\n#define NODE_PATCH_VERSION 1\n"), 0o644)
	tcs := DiscoverNode(context.Background(), []string{root}, host)
	if len(tcs) != 1 {
		t.Fatalf("toolchains %+v", tcs)
	}
	tc := tcs[0]
	if tc.Version != "20.11.1" || !tc.Managed || tc.Source != SrcManaged || tc.Kind != "node" || len(tc.PathPrepend) != 1 || tc.Label != "Node.js 20.11.1" {
		t.Errorf("toolchain %+v", tc)
	}
	// 不在 toolchains 目录下的是普通安装
	if tcs := DiscoverNode(context.Background(), []string{root}, t.TempDir()); len(tcs) != 1 || tcs[0].Managed {
		t.Errorf("unmanaged %+v", tcs)
	}
}
