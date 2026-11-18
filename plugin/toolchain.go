package nodejs

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 工具链来源。
const (
	SrcNvm      = "nvm"
	SrcFnm      = "fnm"
	SrcVolta    = "volta"
	SrcN        = "n"
	SrcAsdf     = "asdf"
	SrcMise     = "mise"
	SrcNodenv   = "nodenv"
	SrcNvs      = "nvs"
	SrcHomebrew = "homebrew"
	SrcScoop    = "scoop"
	SrcSystem   = "system"
	SrcManaged  = "managed"
)

type nodeCandidate struct {
	root   string // 安装根目录（Toolchain.Path）
	source string
}

// nodeCandidates 列出各版本管理器与常见安装位置下的 Node 安装目录（只读）。
func nodeCandidates(goos, home, hostDataDir string, getenv func(string) string) []nodeCandidate {
	var out []nodeCandidate
	add := func(src string, roots ...string) {
		for _, r := range roots {
			out = append(out, nodeCandidate{root: r, source: src})
		}
	}
	glob := func(pattern string) []string {
		m, _ := filepath.Glob(pattern)
		return m
	}
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	if hostDataDir != "" {
		add(SrcManaged, glob(filepath.Join(hostDataDir, "toolchains", "node", "*"))...)
	}
	if goos == "windows" {
		appData := env("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		localAppData := env("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
		if nh := getenv("NVM_HOME"); nh != "" {
			add(SrcNvm, glob(filepath.Join(nh, "v*"))...)
		}
		add(SrcNvm, glob(filepath.Join(appData, "nvm", "v*"))...)
		add(SrcFnm, glob(filepath.Join(appData, "fnm", "node-versions", "*", "installation"))...)
		add(SrcFnm, glob(filepath.Join(env("FNM_DIR", filepath.Join(localAppData, "fnm")), "node-versions", "*", "installation"))...)
		add(SrcVolta, glob(filepath.Join(env("VOLTA_HOME", filepath.Join(localAppData, "Volta")), "tools", "image", "node", "*"))...)
		add(SrcNvs, glob(filepath.Join(env("NVS_HOME", filepath.Join(localAppData, "nvs")), "node", "*", "*"))...)
		add(SrcScoop, glob(filepath.Join(env("SCOOP", filepath.Join(home, "scoop")), "apps", "nodejs*", "current"))...)
		for _, pf := range []string{getenv("ProgramFiles"), getenv("ProgramFiles(x86)"), `C:\Program Files`} {
			if pf != "" {
				add(SrcSystem, filepath.Join(pf, "nodejs"))
			}
		}
		return out
	}
	add(SrcNvm, glob(filepath.Join(env("NVM_DIR", filepath.Join(home, ".nvm")), "versions", "node", "*"))...)
	add(SrcNvm, glob(filepath.Join(home, ".config", "nvm", "versions", "node", "*"))...)
	for _, fnmDir := range []string{getenv("FNM_DIR"), filepath.Join(home, "Library", "Application Support", "fnm"), filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "fnm"), filepath.Join(home, ".fnm")} {
		if fnmDir != "" {
			add(SrcFnm, glob(filepath.Join(fnmDir, "node-versions", "*", "installation"))...)
		}
	}
	add(SrcVolta, glob(filepath.Join(env("VOLTA_HOME", filepath.Join(home, ".volta")), "tools", "image", "node", "*"))...)
	add(SrcN, glob(filepath.Join(env("N_PREFIX", "/usr/local"), "n", "versions", "node", "*"))...)
	add(SrcAsdf, glob(filepath.Join(env("ASDF_DATA_DIR", filepath.Join(home, ".asdf")), "installs", "nodejs", "*"))...)
	miseData := env("MISE_DATA_DIR", filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "mise"))
	add(SrcMise, glob(filepath.Join(miseData, "installs", "node", "*"))...)
	add(SrcMise, glob(filepath.Join(miseData, "installs", "nodejs", "*"))...)
	add(SrcNodenv, glob(filepath.Join(env("NODENV_ROOT", filepath.Join(home, ".nodenv")), "versions", "*"))...)
	add(SrcNvs, glob(filepath.Join(env("NVS_HOME", filepath.Join(home, ".nvs")), "node", "*", "*"))...)
	for _, brew := range []string{"/opt/homebrew/opt", "/usr/local/opt", "/home/linuxbrew/.linuxbrew/opt", filepath.Join(home, ".linuxbrew", "opt")} {
		add(SrcHomebrew, glob(filepath.Join(brew, "node"))...)
		add(SrcHomebrew, glob(filepath.Join(brew, "node@*"))...)
	}
	add(SrcSystem, "/usr/local", "/usr", "/opt/local", "/snap/node/current")
	if goos == "linux" {
		add(SrcSystem, glob("/usr/lib/nodejs*")...)
		add(SrcSystem, glob("/opt/node*")...)
	}
	return out
}

// nodeExe 返回安装根目录下的 node 可执行文件与 bin 目录。
func nodeExe(root, goos string) (exe, bin string) {
	if goos == "windows" {
		return filepath.Join(root, "node.exe"), root
	}
	return filepath.Join(root, "bin", "node"), filepath.Join(root, "bin")
}

var nodeVersionHRe = regexp.MustCompile(`#define NODE_(MAJOR|MINOR|PATCH)_VERSION (\d+)`)
var dirVersionRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)$`)

// probeNodeVersion 依次用 include/node/node_version.h、目录名、node --version 取版本。
func probeNodeVersion(ctx context.Context, root, exe string) string {
	if h := sdk.ReadText(filepath.Join(root, "include", "node", "node_version.h"), 64<<10); h != "" {
		parts := map[string]string{}
		for _, m := range nodeVersionHRe.FindAllStringSubmatch(h, -1) {
			parts[m[1]] = m[2]
		}
		if parts["MAJOR"] != "" && parts["MINOR"] != "" && parts["PATCH"] != "" {
			return parts["MAJOR"] + "." + parts["MINOR"] + "." + parts["PATCH"]
		}
	}
	for _, name := range []string{filepath.Base(root), filepath.Base(filepath.Dir(root))} {
		if m := dirVersionRe.FindStringSubmatch(name); m != nil {
			return m[1]
		}
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, exe, "--version").Output()
	if err != nil {
		return ""
	}
	if v, ok := sdk.ParseVersion(strings.TrimSpace(string(out))); ok {
		return v.String()
	}
	return ""
}

// binaryArch 只读可执行文件开头几 KB 判断架构（Mach-O / 通用二进制 / ELF / PE）。
// 不用 debug/macho：它会把整个符号表读进内存，node 可执行文件一次就要 70 MB 以上。
func binaryArch(exe string) string {
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	return archFromHeader(buf[:n], func(off int64) []byte {
		b := make([]byte, 8)
		if _, err := f.ReadAt(b, off); err != nil {
			return nil
		}
		return b
	})
}

// archFromHeader 解析文件头。readAt 用于 PE 头不在前 4 KB 的情况。
func archFromHeader(h []byte, readAt func(off int64) []byte) string {
	if len(h) < 8 {
		return ""
	}
	le := binary.LittleEndian
	be := binary.BigEndian
	switch {
	case le.Uint32(h) == 0xfeedfacf || le.Uint32(h) == 0xfeedface: // Mach-O 小端
		return machoCPU(le.Uint32(h[4:]))
	case be.Uint32(h) == 0xfeedfacf || be.Uint32(h) == 0xfeedface:
		return machoCPU(be.Uint32(h[4:]))
	case be.Uint32(h) == 0xcafebabe: // 通用二进制：nfat 个 fat_arch，每个 20 字节
		nfat := int(be.Uint32(h[4:]))
		first := ""
		for i := 0; i < nfat && 8+i*20+8 <= len(h); i++ {
			a := machoCPU(be.Uint32(h[8+i*20:]))
			if a == runtime.GOARCH {
				return a
			}
			if first == "" {
				first = a
			}
		}
		return first
	case string(h[:4]) == "\x7fELF" && len(h) >= 20:
		var m uint16
		if h[5] == 2 {
			m = be.Uint16(h[18:])
		} else {
			m = le.Uint16(h[18:])
		}
		switch m {
		case 62:
			return "amd64"
		case 183:
			return "arm64"
		case 40:
			return "arm"
		case 3:
			return "386"
		}
	case h[0] == 'M' && h[1] == 'Z' && len(h) >= 0x40:
		off := int64(le.Uint32(h[0x3c:]))
		var pe []byte
		if off+6 <= int64(len(h)) {
			pe = h[off : off+6]
		} else if readAt != nil {
			pe = readAt(off)
		}
		if len(pe) >= 6 && string(pe[:4]) == "PE\x00\x00" {
			switch le.Uint16(pe[4:]) {
			case 0x8664:
				return "amd64"
			case 0xaa64:
				return "arm64"
			case 0x14c:
				return "386"
			}
		}
	}
	return ""
}

func machoCPU(c uint32) string {
	switch c {
	case 0x0100000c:
		return "arm64"
	case 0x01000007:
		return "amd64"
	case 7:
		return "386"
	}
	return ""
}

// DiscoverNode 发现本机 Node 安装。paths 非空时只检查这些目录。
func DiscoverNode(ctx context.Context, paths []string, hostDataDir string) []sdk.Toolchain {
	goos := runtime.GOOS
	var cands []nodeCandidate
	if len(paths) > 0 {
		for _, p := range paths {
			src := SrcSystem
			if hostDataDir != "" && isUnder(p, filepath.Join(hostDataDir, "toolchains")) {
				src = SrcManaged
			}
			cands = append(cands, nodeCandidate{root: p, source: src})
		}
	} else {
		cands = nodeCandidates(goos, sdk.HomeDir(), hostDataDir, os.Getenv)
		// PATH 里的 node（可能是上面没覆盖到的位置）
		if p, err := exec.LookPath("node"); err == nil {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				root := filepath.Dir(real)
				if goos != "windows" {
					root = filepath.Dir(root)
				}
				cands = append(cands, nodeCandidate{root: root, source: SrcSystem})
			}
		}
	}
	seen := map[string]bool{}
	var out []sdk.Toolchain
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		exe, bin := nodeExe(c.root, goos)
		st, err := os.Stat(exe)
		if err != nil || st.IsDir() {
			continue
		}
		real, err := filepath.EvalSymlinks(exe)
		if err != nil {
			real = exe
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		src := c.source
		if src == SrcSystem && strings.Contains(real, "/Cellar/node") {
			src = SrcHomebrew
		}
		// /usr/local/bin/node 指向 Homebrew 时已在上面按 opt 路径收录
		ver := probeNodeVersion(ctx, c.root, exe)
		if ver == "" {
			continue
		}
		tc := sdk.Toolchain{
			Kind:        "node",
			Version:     ver,
			Vendor:      "Node.js",
			Arch:        binaryArch(real),
			Path:        c.root,
			Executable:  exe,
			PathPrepend: []string{bin},
			Source:      src,
			Managed:     src == SrcManaged,
			Label:       "Node.js " + ver,
		}
		out = append(out, tc)
	}
	sort.SliceStable(out, func(i, j int) bool { return sdk.CompareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

func isUnder(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// nodeMajor 返回工具链主版本（未知为 0）。
func nodeMajor(tc *sdk.Toolchain) int {
	if tc == nil {
		return 0
	}
	v, _ := sdk.ParseVersion(tc.Version)
	return v.Major()
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
