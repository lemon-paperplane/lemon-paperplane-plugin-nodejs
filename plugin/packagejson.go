package nodejs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PackageJSON 是宽松解析后的 package.json。字段类型不规范（脚本不是字符串、依赖写成数组等）时跳过该项而不是整体失败。
type PackageJSON struct {
	Name                 string
	Version              string
	Private              bool
	Type                 string
	Scripts              map[string]string
	ScriptOrder          []string // 按文件中出现的顺序
	Dependencies         map[string]string
	DevDependencies      map[string]string
	OptionalDependencies map[string]string
	PeerDependencies     map[string]string
	Engines              map[string]string
	Volta                map[string]string
	PackageManager       string
	Workspaces           []string
	UniModules           bool // uni_modules 插件包（不是工程）
	UnityPackage         bool // Unity 包清单（UPM，有 "unity" 字段），不是 npm 工程
}

// ReadPackageJSON 读取 dir/package.json。
func ReadPackageJSON(dir string) (*PackageJSON, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	return ParsePackageJSON(b)
}

// ParsePackageJSON 解析 package.json 内容。
func ParsePackageJSON(b []byte) (*PackageJSON, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	p := &PackageJSON{}
	p.Name = rawString(raw["name"])
	p.Version = rawString(raw["version"])
	p.Type = rawString(raw["type"])
	p.PackageManager = strings.TrimSpace(rawString(raw["packageManager"]))
	_ = json.Unmarshal(raw["private"], &p.Private)
	p.Scripts = rawStringMap(raw["scripts"])
	p.ScriptOrder = orderedKeys(raw["scripts"], p.Scripts)
	p.Dependencies = rawStringMap(raw["dependencies"])
	p.DevDependencies = rawStringMap(raw["devDependencies"])
	p.OptionalDependencies = rawStringMap(raw["optionalDependencies"])
	p.PeerDependencies = rawStringMap(raw["peerDependencies"])
	p.Engines = rawStringMap(raw["engines"])
	p.Volta = rawStringMap(raw["volta"])
	p.Workspaces = parseWorkspaces(raw["workspaces"])
	_, p.UniModules = raw["uni_modules"]
	_, p.UnityPackage = raw["unity"]
	if _, ok := raw["dcloudext"]; ok {
		p.UniModules = true
	}
	return p, nil
}

func rawString(r json.RawMessage) string {
	var s string
	if len(r) > 0 && json.Unmarshal(r, &s) == nil {
		return s
	}
	return ""
}

func rawStringMap(r json.RawMessage) map[string]string {
	if len(r) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(r, &m) != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
		}
	}
	return out
}

// orderedKeys 按出现顺序取出 JSON 对象的键（只保留 valid 里存在的）。
func orderedKeys(r json.RawMessage, valid map[string]string) []string {
	if len(r) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(r))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return sortedKeys(valid)
	}
	var out []string
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			break
		}
		k, _ := t.(string)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			break
		}
		if _, ok := valid[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseWorkspaces 支持 ["packages/*"] 与 {"packages": [...]} 两种写法。
func parseWorkspaces(r json.RawMessage) []string {
	if len(r) == 0 {
		return nil
	}
	var arr []string
	if json.Unmarshal(r, &arr) == nil {
		return arr
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if json.Unmarshal(r, &obj) == nil {
		return obj.Packages
	}
	return nil
}

// AllDeps 返回 dependencies + devDependencies + optionalDependencies 的合并（不含 peer）。
func (p *PackageJSON) AllDeps() map[string]string {
	out := map[string]string{}
	for _, m := range []map[string]string{p.OptionalDependencies, p.DevDependencies, p.Dependencies} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// HasDep 判断是否声明了某个依赖（含 dev/optional/peer）。
func (p *PackageJSON) HasDep(name string) bool {
	if p == nil {
		return false
	}
	for _, m := range []map[string]string{p.Dependencies, p.DevDependencies, p.OptionalDependencies, p.PeerDependencies} {
		if _, ok := m[name]; ok {
			return true
		}
	}
	return false
}

// DepRange 返回依赖声明的版本范围。
func (p *PackageJSON) DepRange(name string) string {
	for _, m := range []map[string]string{p.Dependencies, p.DevDependencies, p.OptionalDependencies, p.PeerDependencies} {
		if v, ok := m[name]; ok {
			return v
		}
	}
	return ""
}

// HasAnyDeps 判断是否声明了需要安装的依赖。
func (p *PackageJSON) HasAnyDeps() bool {
	return len(p.Dependencies)+len(p.DevDependencies)+len(p.OptionalDependencies) > 0
}

// lifecycleScripts 是包管理器自动触发的生命周期脚本，不作为运行配置。
var lifecycleScripts = map[string]bool{
	"preinstall": true, "install": true, "postinstall": true,
	"prepublish": true, "preprepare": true, "prepare": true, "postprepare": true,
	"prepublishOnly": true, "prepack": true, "postpack": true, "publish": true, "postpublish": true,
	"preversion": true, "version": true, "postversion": true,
	"dependencies": true, "preuninstall": true, "uninstall": true, "postuninstall": true,
	"prerestart": true, "restart": true, "postrestart": true,
	"prestop": true, "stop": true, "poststop": true, "preshrinkwrap": true, "shrinkwrap": true, "postshrinkwrap": true,
}

// IsLifecycleScript 判断脚本是否是生命周期脚本（包括 pre<x>/post<x> 钩子）。
func (p *PackageJSON) IsLifecycleScript(name string) bool {
	if lifecycleScripts[name] {
		return true
	}
	for _, prefix := range []string{"pre", "post"} {
		if strings.HasPrefix(name, prefix) {
			if _, ok := p.Scripts[strings.TrimPrefix(name, prefix)]; ok {
				return true
			}
		}
	}
	return false
}

// RunnableScripts 返回可作为运行配置的脚本（按文件顺序）。
func (p *PackageJSON) RunnableScripts() []string {
	var out []string
	for _, s := range p.ScriptOrder {
		if !p.IsLifecycleScript(s) && strings.TrimSpace(p.Scripts[s]) != "" {
			out = append(out, s)
		}
	}
	return out
}

// templateNames 是脚手架默认名字，用它当模块名没有意义，改用目录名。
var templateNames = map[string]bool{
	"uni-preset-vue": true, "vite-project": true, "vue-project": true, "my-vue-app": true,
	"my-react-app": true, "my-app": true, "react-app": true, "vite-react": true, "hello-world": true,
	"nuxt-app": true, "my-next-app": true, "vue3-project": true, "default": true,
}

// DisplayName 返回模块名建议。
func (p *PackageJSON) DisplayName(dir string) string {
	if p == nil || p.Name == "" || templateNames[p.Name] {
		return filepath.Base(dir)
	}
	return p.Name
}

// cleanRange 从声明的版本范围里取出可用于比较的最低版本：^6.0.6 → 6.0.6，npm:vite@^5 → 5。
func cleanRange(r string) string {
	r = strings.TrimSpace(r)
	if i := strings.LastIndex(r, "@"); strings.HasPrefix(r, "npm:") && i > 0 {
		r = r[i+1:]
	}
	for _, bad := range []string{"workspace:", "file:", "link:", "portal:", "git", "http:", "https:", "github:"} {
		if strings.HasPrefix(r, bad) {
			return ""
		}
	}
	if r == "" || r == "*" || r == "latest" || r == "next" || r == "x" {
		return ""
	}
	// 只取第一个 || 分支里的第一个比较项
	if i := strings.Index(r, "||"); i >= 0 {
		r = strings.TrimSpace(r[:i])
	}
	if f := strings.Fields(r); len(f) > 0 {
		r = f[0]
	}
	r = strings.TrimLeft(r, "^~=>v ")
	r = strings.TrimSuffix(strings.TrimSuffix(r, ".x"), ".*")
	r = strings.TrimSuffix(strings.TrimSuffix(r, ".x"), ".*")
	return r
}
