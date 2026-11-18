package nodejs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 依赖状态。
const (
	DepsOK       = "ok"
	DepsMissing  = "missing"
	DepsOutdated = "outdated"
	DepsNone     = "none"
	DepsUnknown  = "unknown"
)

// mtimeSlack：锁文件比安装标记新出这么多才算过期（git 检出、编辑器保存都会动修改时间）。
const mtimeSlack = 2 * time.Second

// DepStatus 是依赖状态的详细结果。
type DepStatus struct {
	State   string
	Missing []string // 缺少的直接依赖
	Reason  string   // outdated 的原因 key
}

// CheckDeps 判断依赖安装状态。deps 是 CollectDeps 的结果。
func CheckDeps(dir string, pkg *PackageJSON, pm PMInfo, deps []DepInfo) DepStatus {
	if pkg == nil {
		return DepStatus{State: DepsUnknown}
	}
	if !pkg.HasAnyDeps() {
		return DepStatus{State: DepsNone}
	}
	// yarn berry PnP：没有 node_modules
	if pm.Name == PMYarn && pm.Berry && (sdk.FileExists(filepath.Join(dir, ".pnp.cjs")) || sdk.FileExists(filepath.Join(dir, ".pnp.js"))) {
		state := filepath.Join(dir, ".yarn", "install-state.gz")
		if !sdk.FileExists(state) {
			return DepStatus{State: DepsMissing}
		}
		if newerThan(filepath.Join(dir, pm.Lockfile), state) {
			return DepStatus{State: DepsOutdated, Reason: "lockfile"}
		}
		return DepStatus{State: DepsOK}
	}
	nm := filepath.Join(dir, "node_modules")
	if !sdk.DirExists(nm) {
		return DepStatus{State: DepsMissing}
	}
	var missing []string
	for _, d := range deps {
		if d.Optional || d.Installed != "" {
			continue
		}
		// 本地链接依赖可能没有 package.json 版本号，但目录在
		if sdk.DirExists(filepath.Join(nm, filepath.FromSlash(d.Name))) {
			continue
		}
		missing = append(missing, d.Name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return DepStatus{State: DepsMissing, Missing: missing}
	}
	lock := ""
	if pm.Lockfile != "" {
		lock = filepath.Join(dir, pm.Lockfile)
	}
	switch pm.Name {
	case PMNpm:
		marker := filepath.Join(nm, ".package-lock.json")
		if lock != "" && sdk.FileExists(marker) && newerThan(lock, marker) {
			return DepStatus{State: DepsOutdated, Reason: "lockfile"}
		}
	case PMYarn:
		if pm.Berry {
			marker := filepath.Join(nm, ".yarn-state.yml")
			if lock != "" && sdk.FileExists(marker) && newerThan(lock, marker) {
				return DepStatus{State: DepsOutdated, Reason: "lockfile"}
			}
			break
		}
		integrity := filepath.Join(nm, ".yarn-integrity")
		if sdk.FileExists(integrity) {
			if !yarnIntegrityMatches(integrity, pkg) {
				return DepStatus{State: DepsOutdated, Reason: "packageJson"}
			}
			if lock != "" && newerThan(lock, integrity) {
				return DepStatus{State: DepsOutdated, Reason: "lockfile"}
			}
		}
	case PMPnpm:
		marker := filepath.Join(nm, ".modules.yaml")
		if lock != "" && sdk.FileExists(marker) && newerThan(lock, marker) {
			// pnpm 在 node_modules/.pnpm/lock.yaml 保存安装时的锁文件副本，内容相同就不算过期
			if !sameFile(lock, filepath.Join(nm, ".pnpm", "lock.yaml")) {
				return DepStatus{State: DepsOutdated, Reason: "lockfile"}
			}
		}
	case PMBun:
		if lock != "" && newerThan(lock, nm) {
			return DepStatus{State: DepsOutdated, Reason: "lockfile"}
		}
	}
	return DepStatus{State: DepsOK}
}

func newerThan(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return sa.ModTime().Sub(sb.ModTime()) > mtimeSlack
}

// sameFile 逐块比较两个文件（先比大小）。
func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil || sa.Size() != sb.Size() {
		return false
	}
	fa, err1 := os.Open(a)
	fb, err2 := os.Open(b)
	if err1 != nil || err2 != nil {
		return false
	}
	defer fa.Close()
	defer fb.Close()
	ba, bb := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, _ := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if ea != nil {
			return true
		}
	}
}

// readTopLevelPatterns 流式读取 .yarn-integrity 的 topLevelPatterns（文件里还有全部锁文件条目，可能很大）。
func readTopLevelPatterns(path string) ([]string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 32<<10))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		if k, _ := t.(string); k == "topLevelPatterns" {
			var pats []string
			if dec.Decode(&pats) != nil {
				return nil, false
			}
			return pats, true
		}
		if skipValue(dec) != nil {
			return nil, false
		}
	}
	return nil, false
}

// yarnIntegrityMatches 比较 .yarn-integrity 的 topLevelPatterns 与 package.json 声明是否一致。
func yarnIntegrityMatches(path string, pkg *PackageJSON) bool {
	pats, ok := readTopLevelPatterns(path)
	if !ok {
		return true
	}
	have := map[string]bool{}
	for _, p := range pats {
		have[p] = true
	}
	want := map[string]bool{}
	for name, r := range pkg.AllDeps() {
		if strings.HasPrefix(r, "workspace:") || strings.HasPrefix(r, "file:") || strings.HasPrefix(r, "link:") {
			continue
		}
		want[name+"@"+r] = true
	}
	for p := range want {
		if !have[p] {
			return false
		}
	}
	for p := range have {
		if !want[p] && !strings.Contains(p, "@file:") && !strings.Contains(p, "@link:") {
			return false
		}
	}
	return true
}
