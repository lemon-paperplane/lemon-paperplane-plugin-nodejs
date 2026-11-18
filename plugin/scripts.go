package nodejs

import (
	"path/filepath"
	"regexp"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// RunTypeNpmScript 是运行配置类型 id。
const RunTypeNpmScript = "npm-script"

// scriptHints 是框架插件经 facet Data 传来的脚本偏好（键见 SDK 的 FacetDataPreferScripts /
// FacetDataScriptTitles / FacetDataHideScripts）。
type scriptHints struct {
	prefer []string
	titles map[string]string
	hide   map[string]bool
}

func facetHints(facets []sdk.Facet) scriptHints {
	h := scriptHints{titles: map[string]string{}, hide: map[string]bool{}}
	for _, f := range facets {
		if f.ID == "nodejs" || f.Data == nil {
			continue
		}
		h.prefer = append(h.prefer, anyStrings(f.Data[sdk.FacetDataPreferScripts])...)
		for _, s := range anyStrings(f.Data[sdk.FacetDataHideScripts]) {
			h.hide[s] = true
		}
		if m, ok := f.Data[sdk.FacetDataScriptTitles].(map[string]any); ok {
			for k, v := range m {
				if s, ok := v.(string); ok && s != "" {
					h.titles[k] = s
				}
			}
		}
	}
	return h
}

func anyStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// 名字优先级：dev > start > serve > dev:h5 > …
var scriptNameScore = map[string]int{
	"dev": 100, "start": 92, "serve": 88, "dev:h5": 84, "develop": 82, "start:dev": 80, "dev:web": 79,
	"server": 70, "watch": 55, "preview": 35, "build": 45, "test": 25, "lint": 15, "format": 10,
}

var (
	// devServerRes：命令里出现这些就像是「开发运行」
	devServerRes = []*regexp.Regexp{
		regexp.MustCompile(`\bvite\b`),
		regexp.MustCompile(`vue-cli-service\s+(?:serve|uni-serve)\b`),
		regexp.MustCompile(`\b(?:next|nuxt|nuxi|astro|remix|react-router)\s+dev\b`),
		regexp.MustCompile(`react-scripts\s+start\b`),
		regexp.MustCompile(`webpack\s+serve\b|webpack-dev-server`),
		regexp.MustCompile(`\b(?:nodemon|ts-node-dev)\b|\btsx\s+watch\b`),
		regexp.MustCompile(`\bng\s+serve\b`),
		regexp.MustCompile(`(?:^|&&\s*|;\s*)uni(?:\s+-p\s+\S+)?\s*$`),
		regexp.MustCompile(`\bnode\s+\S+\.(?:m?js|cjs)\b`),
	}
	// buildOnlyRe：一次性构建 / 检查类命令
	buildOnlyRe = regexp.MustCompile(`\bvite\s+(?:build|preview|optimize)\b|vue-cli-service\s+(?:build|uni-build|lint)\b|\b(?:next|nuxt|nuxi)\s+(?:build|generate|lint)\b|react-scripts\s+(?:build|test)\b|webpack\s+--mode\s+production|\btsc\b|vue-tsc|\beslint\b|\bprettier\b|\buni\s+build\b|\bjest\b|\bvitest\b|\bplaywright\b|\bcypress\b`)
)

func looksLikeDevServer(cmd string) bool {
	if buildOnlyRe.MatchString(cmd) {
		return false
	}
	for _, re := range devServerRes {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// scriptScore 综合名字与命令判断脚本是不是「开发运行」类，分数越高越适合作默认。
func scriptScore(name, cmd string) int {
	score, ok := scriptNameScore[name]
	if !ok {
		switch {
		case strings.HasPrefix(name, "dev"):
			score = 60
		case strings.HasPrefix(name, "start"), strings.HasPrefix(name, "serve"):
			score = 58
		case strings.HasPrefix(name, "build"):
			score = 30
		case strings.HasPrefix(name, "test"), strings.HasPrefix(name, "lint"), strings.HasPrefix(name, "type"), strings.HasPrefix(name, "check"):
			score = 12
		default:
			score = 20
		}
	}
	isBuild := buildOnlyRe.MatchString(cmd)
	if looksLikeDevServer(cmd) {
		score += 15
	}
	if isBuild && !strings.HasPrefix(name, "build") {
		score -= 20
	}
	return score
}

// PickDefaultScript 选默认脚本：框架偏好 → 分数最高（开发类）→ 第一个可运行脚本。
func PickDefaultScript(pkg *PackageJSON, prefer []string, hide map[string]bool) string {
	runnable := pkg.RunnableScripts()
	has := map[string]bool{}
	for _, s := range runnable {
		if !hide[s] {
			has[s] = true
		}
	}
	for _, s := range prefer {
		if has[s] {
			return s
		}
	}
	best, bestScore := "", -1000
	for _, s := range runnable {
		if !has[s] {
			continue
		}
		if sc := scriptScore(s, pkg.Scripts[s]); sc > bestScore {
			best, bestScore = s, sc
		}
	}
	return best
}

// buildRunConfigs 为根 package.json 的每个可运行脚本生成一条运行配置；workspace 成员只生成它的默认脚本。
func buildRunConfigs(ctx *sdk.Context, mod sdk.ModuleInfo, pkg *PackageJSON, members []string, hints scriptHints) ([]sdk.RunConfiguration, string) {
	def := PickDefaultScript(pkg, hints.prefer, hints.hide)
	env := map[string]string{"FORCE_COLOR": "1", "BROWSER": "none"}
	var out []sdk.RunConfiguration
	for _, s := range pkg.RunnableScripts() {
		if hints.hide[s] {
			continue
		}
		name := s
		if t := hints.titles[s]; t != "" {
			name = t
		}
		rc := sdk.RunConfiguration{
			ID:       sdk.StableID(mod.RelDir, RunTypeNpmScript, "", s),
			Name:     name,
			Type:     RunTypeNpmScript,
			Options:  map[string]any{"script": s},
			Env:      copyEnv(env),
			Default:  s == def,
			Source:   "auto",
			Priority: scriptScore(s, pkg.Scripts[s]),
		}
		if rc.Default {
			rc.Priority = 200
		}
		out = append(out, rc)
	}
	for _, m := range members {
		mp, err := ReadPackageJSON(filepath.Join(mod.Dir, filepath.FromSlash(m)))
		if err != nil {
			continue
		}
		s := PickDefaultScript(mp, nil, nil)
		if s == "" || scriptScore(s, mp.Scripts[s]) < 50 {
			continue
		}
		label := mp.Name
		if label == "" {
			label = m
		}
		out = append(out, sdk.RunConfiguration{
			ID:       sdk.StableID(mod.RelDir, RunTypeNpmScript, m, s),
			Name:     label + ": " + s,
			Type:     RunTypeNpmScript,
			Options:  map[string]any{"script": s, "packageDir": m},
			Env:      copyEnv(env),
			Source:   "auto",
			Priority: scriptScore(s, mp.Scripts[s]) - 30,
		})
	}
	return out, def
}

func copyEnv(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
