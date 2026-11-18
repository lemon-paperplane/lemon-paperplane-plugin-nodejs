package nodejs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// 版本约束求交与化简。
//
// 约束用 npm semver 范围语法，判断交给 SDK 的 ParseConstraint。这里在「关键点」上逐段求值：
// 所有约束里出现的版本号及其后继（主/次/补丁 +1）把版本轴切成若干段，同一段内真假不变，
// 于是能精确算出交集，再渲染成简短的范围（如 "^20.19 || >=22.12"）。

type ver3 [3]int

func (v ver3) less(o ver3) bool {
	for i := 0; i < 3; i++ {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

func (v ver3) sdk() sdk.Version {
	return sdk.Version{Parts: [4]int{v[0], v[1], v[2], 0}, N: 3}
}

func (v ver3) succ() ver3 { return ver3{v[0], v[1], v[2] + 1} }

func (v ver3) String() string {
	switch {
	case v[1] == 0 && v[2] == 0:
		return fmt.Sprint(v[0])
	case v[2] == 0:
		return fmt.Sprintf("%d.%d", v[0], v[1])
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

var numTokRe = regexp.MustCompile(`\d+(?:\.\d+){0,2}`)

const verInf = 100000

// IntersectConstraints 求若干约束的交集。空串或 "*" 视为任意。返回化简后的约束（任意时为 ""）与是否可满足。
func IntersectConstraints(cs ...string) (string, bool) {
	var parsed []sdk.Constraint
	var raws []string
	for _, c := range cs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		pc := sdk.ParseConstraint(c)
		if pc.Any() {
			continue
		}
		parsed = append(parsed, pc)
		raws = append(raws, c)
	}
	if len(parsed) == 0 {
		return "", true
	}
	pred := func(v ver3) bool {
		sv := v.sdk()
		for _, c := range parsed {
			if !c.Check(sv) {
				return false
			}
		}
		return true
	}
	ranges := evalRanges(raws, pred)
	if len(ranges) == 0 {
		return "", false
	}
	return renderRanges(ranges), true
}

// Satisfiable 判断约束是否有解。
func Satisfiable(cs ...string) bool {
	_, ok := IntersectConstraints(cs...)
	return ok
}

// AllowsMajor 判断约束是否允许某个主版本中的至少一个版本。
func AllowsMajor(constraint string, major int) bool {
	return Satisfiable(constraint, fmt.Sprint(major))
}

// vrange 是 [lo, hi) 区间；hiInf 表示无上界。
type vrange struct {
	lo, hi ver3
	hiInf  bool
}

func evalRanges(raws []string, pred func(ver3) bool) []vrange {
	set := map[ver3]bool{{0, 0, 0}: true}
	for _, r := range raws {
		for _, tok := range numTokRe.FindAllString(r, -1) {
			var v ver3
			parts := strings.Split(tok, ".")
			for i := 0; i < len(parts) && i < 3; i++ {
				fmt.Sscan(parts[i], &v[i])
			}
			set[v] = true
			set[ver3{v[0] + 1, 0, 0}] = true
			set[ver3{v[0], v[1] + 1, 0}] = true
			set[v.succ()] = true
		}
	}
	pts := make([]ver3, 0, len(set)+1)
	for v := range set {
		pts = append(pts, v)
	}
	pts = append(pts, ver3{verInf, 0, 0})
	sort.Slice(pts, func(i, j int) bool { return pts[i].less(pts[j]) })

	// 段：每个关键点本身 + 它与下一个关键点之间的开区间
	type seg struct {
		at   ver3 // 段起点（含）
		ok   bool
		open bool // 是否是开区间段
	}
	var segs []seg
	for i := 0; i < len(pts)-1; i++ {
		segs = append(segs, seg{at: pts[i], ok: pred(pts[i])})
		if s := pts[i].succ(); s.less(pts[i+1]) {
			segs = append(segs, seg{at: s, ok: pred(s), open: true})
		}
	}
	var out []vrange
	in := false
	var cur vrange
	for _, s := range segs {
		if s.ok && !in {
			in, cur = true, vrange{lo: s.at}
		} else if !s.ok && in {
			in = false
			cur.hi = s.at
			out = append(out, cur)
		}
	}
	if in {
		cur.hiInf = true
		out = append(out, cur)
	}
	return out
}

func renderRanges(rs []vrange) string {
	var parts []string
	for _, r := range rs {
		switch {
		case r.lo == ver3{} && r.hiInf:
			return ""
		case r.hiInf:
			parts = append(parts, ">="+r.lo.String())
		case r.lo == ver3{}:
			parts = append(parts, "<"+r.hi.String())
		case r.hi == ver3{r.lo[0] + 1, 0, 0} && r.lo[0] > 0:
			if r.lo[1] == 0 && r.lo[2] == 0 {
				parts = append(parts, r.lo.String())
			} else {
				parts = append(parts, "^"+r.lo.String())
			}
		case r.hi == r.lo.succ():
			parts = append(parts, fmt.Sprintf("%d.%d.%d", r.lo[0], r.lo[1], r.lo[2]))
		default:
			parts = append(parts, ">="+r.lo.String()+" <"+r.hi.String())
		}
	}
	return strings.Join(parts, " || ")
}

// lowestMajor 返回约束允许的最低主版本（不可满足返回 -1）。
func lowestMajor(c string) int {
	if strings.TrimSpace(c) == "" {
		return 0
	}
	pc := sdk.ParseConstraint(c)
	rs := evalRanges([]string{c}, func(v ver3) bool { return pc.Check(v.sdk()) })
	if len(rs) == 0 {
		return -1
	}
	return rs[0].lo[0]
}
