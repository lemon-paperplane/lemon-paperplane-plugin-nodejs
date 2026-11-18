package nodejs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DepInfo 是一个直接依赖的实际情况。
type DepInfo struct {
	Name      string
	Declared  string
	Dev       bool
	Optional  bool
	Installed string // node_modules 里的版本，未安装为空
	Locked    string // 锁文件里解析到的版本
	Engine    string // engines.node
}

// Version 返回最能代表实际使用的版本：已安装 > 锁文件 > 声明范围下限。
func (d DepInfo) Version() string {
	switch {
	case d.Installed != "":
		return d.Installed
	case d.Locked != "":
		return d.Locked
	}
	return cleanRange(d.Declared)
}

type installedPkg struct {
	Version string            `json:"version"`
	Engines json.RawMessage   `json:"engines"`
	Bin     json.RawMessage   `json:"bin"`
	Extra   map[string]string `json:"-"`
}

// engineNode 兼容 engines 写成对象或数组（很老的包）。
func engineNode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		return rawString(m["node"])
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, s := range arr {
			if strings.HasPrefix(s, "node ") {
				return strings.TrimSpace(strings.TrimPrefix(s, "node"))
			}
		}
	}
	return ""
}

// CollectDeps 收集直接依赖的声明、安装与锁定版本。
func CollectDeps(dir string, pkg *PackageJSON, pm PMInfo) []DepInfo {
	if pkg == nil {
		return nil
	}
	var out []DepInfo
	seen := map[string]bool{}
	add := func(m map[string]string, dev, opt bool) {
		for _, name := range sortedKeys(m) {
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, DepInfo{Name: name, Declared: m[name], Dev: dev, Optional: opt})
		}
	}
	add(pkg.Dependencies, false, false)
	add(pkg.OptionalDependencies, false, true)
	add(pkg.DevDependencies, true, false)

	locked := lockedVersions(dir, pm, pkg)
	for i := range out {
		d := &out[i]
		if ip := readInstalled(dir, d.Name); ip != nil {
			d.Installed = ip.Version
			d.Engine = engineNode(ip.Engines)
		}
		if lv, ok := locked[d.Name]; ok {
			d.Locked = lv.version
			if d.Engine == "" {
				d.Engine = lv.engine
			}
		}
	}
	return out
}

func readInstalled(dir, name string) *installedPkg {
	b, err := os.ReadFile(filepath.Join(dir, "node_modules", filepath.FromSlash(name), "package.json"))
	if err != nil {
		return nil
	}
	var ip installedPkg
	if json.Unmarshal(b, &ip) != nil {
		return nil
	}
	return &ip
}

type lockedVer struct{ version, engine string }

// lockedVersions 从锁文件读出直接依赖解析到的版本（只读、只解析需要的部分）。
func lockedVersions(dir string, pm PMInfo, pkg *PackageJSON) map[string]lockedVer {
	out := map[string]lockedVer{}
	if pm.Lockfile == "" {
		return out
	}
	path := filepath.Join(dir, pm.Lockfile)
	switch pm.Lockfile {
	case "package-lock.json", "npm-shrinkwrap.json":
		want := map[string]bool{}
		for name := range pkg.AllDeps() {
			want[name] = true
		}
		parseNpmLock(path, want, out)
	case "yarn.lock":
		parseYarnLock(path, pkg, out)
	case "pnpm-lock.yaml":
		parsePnpmLock(path, out)
	case "bun.lock":
		parseBunLock(path, out)
	}
	return out
}

// parseNpmLock 流式读取 package-lock.json，只保留直接依赖（node_modules/<名字>）的版本与 engines，
// 不把整个锁文件读进内存（大工程的锁文件有几 MB）。
func parseNpmLock(path string, want map[string]bool, out map[string]lockedVer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 64<<10))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return
	}
	type entry struct {
		Version string          `json:"version"`
		Engines json.RawMessage `json:"engines"`
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return
		}
		key, _ := t.(string)
		if key != "packages" && key != "dependencies" {
			if skipValue(dec) != nil {
				return
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return
		}
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return
			}
			k, _ := t.(string)
			name := k
			if key == "packages" {
				name = strings.TrimPrefix(k, "node_modules/")
				if name == k {
					name = ""
				}
			}
			if name == "" || !want[name] {
				if skipValue(dec) != nil {
					return
				}
				continue
			}
			var e entry
			if dec.Decode(&e) != nil {
				return
			}
			if _, done := out[name]; !done || key == "packages" {
				out[name] = lockedVer{version: e.Version, engine: engineNode(e.Engines)}
			}
		}
		if _, err := dec.Token(); err != nil { // 结束的 }
			return
		}
	}
}

// skipValue 跳过一个 JSON 值，不分配整段内容。
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

// parseYarnLock 解析 yarn v1 与 berry 锁文件里与直接依赖声明匹配的条目。
func parseYarnLock(path string, pkg *PackageJSON, out map[string]lockedVer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	want := map[string]string{} // "name@range" → name
	for name, r := range pkg.AllDeps() {
		want[name+"@"+r] = name
		want[name+"@npm:"+r] = name
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var current []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] != ' ' {
			current = current[:0]
			head := strings.TrimSuffix(strings.TrimSpace(line), ":")
			for _, spec := range strings.Split(head, ",") {
				spec = strings.Trim(strings.TrimSpace(spec), `"`)
				if name, ok := want[spec]; ok {
					current = append(current, name)
				}
			}
			continue
		}
		if len(current) == 0 {
			continue
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "version ") || strings.HasPrefix(t, "version:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "version"), ":")), `"`)
			for _, n := range current {
				if _, ok := out[n]; !ok {
					out[n] = lockedVer{version: v}
				}
			}
			current = current[:0]
		}
	}
}

var pnpmVerRe = regexp.MustCompile(`^([^(\s]+)`)

var pnpmDepHeaders = map[string]bool{"dependencies:": true, "devDependencies:": true, "optionalDependencies:": true}

// parsePnpmLock 读取直接依赖的版本：v6/v9 的 importers['.'] 或顶层 dependencies（v6 为 name/specifier/version 三行，v5 为 name: version）。
func parsePnpmLock(path string, out map[string]lockedVer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	inImporters, inRoot, inDeps := false, false, false
	nameIndent := 2
	last := ""
	setVer := func(name, v string) {
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		if strings.HasPrefix(v, "link:") || strings.HasPrefix(v, "file:") {
			return
		}
		if m := pnpmVerRe.FindStringSubmatch(v); m != nil {
			out[name] = lockedVer{version: strings.SplitN(m[1], "_", 2)[0]}
		}
	}
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 0:
			inImporters = t == "importers:"
			inRoot = false
			inDeps = pnpmDepHeaders[t]
			nameIndent = 2
			continue
		case inImporters && indent == 2:
			inRoot = t == ".:" || t == "'.':"
			inDeps = false
			continue
		case inImporters && indent == 4:
			inDeps = inRoot && pnpmDepHeaders[t]
			nameIndent = 6
			continue
		}
		if !inDeps {
			continue
		}
		switch {
		case indent == nameIndent:
			name, val, _ := strings.Cut(t, ":")
			name = strings.Trim(name, `'"`)
			last = name
			if strings.TrimSpace(val) != "" {
				setVer(name, val)
			}
		case indent == nameIndent+2 && strings.HasPrefix(t, "version:") && last != "":
			setVer(last, strings.TrimPrefix(t, "version:"))
		}
	}
}

var bunLockRe = regexp.MustCompile(`"([^"]+)":\s*\["((?:@[^@"]+/)?[^@"]+)@([^"]+)"`)

func parseBunLock(path string, out map[string]lockedVer) {
	for _, m := range bunLockRe.FindAllStringSubmatch(readHead(path, 8<<20), -1) {
		if m[1] == m[2] {
			out[m[1]] = lockedVer{version: m[3]}
		}
	}
}

func readHead(path string, max int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, max)
	n, _ := f.Read(buf)
	return string(buf[:n])
}
