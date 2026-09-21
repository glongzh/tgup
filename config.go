package main

// 配置文件：yaml/toml/json，支持一层层的分节写法与命名档位（profiles）。
// 优先级：命令行显式参数 > 配置文件（含 profile） > 内置默认值。
// 对应 tgup.py 的「配置文件」一节。
//
// 这一层的函数一律返回 error 而不是直接 fatal：
// 配置解析是最容易出错、也最值得写单测的地方，
// 一旦内部 os.Exit(1)，测试就无从下手。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// 只用于 --dump-config 的分组与排版，不影响解析
var configSections = []struct {
	Title string
	Keys  []string
}{
	{"目标与输入", []string{"to", "inputs"}},
	{"文件筛选", []string{"recursive", "ext", "min_size", "max_size", "sort", "reverse"}},
	{"上行整形（规避 PCDN 判定的核心）",
		[]string{"rate", "burst", "rest", "window", "daily_cap", "on_cap", "gap_min", "gap_max"}},
	{"限速自检", []string{"no_adaptive", "throttle_floor", "probe_window", "cooldown", "min_rate"}},
	{"超限视频切割", []string{"no_split", "split_dir", "keep_splits"}},
	{"发送选项", []string{"caption", "name_caption", "parse_mode", "force_document",
		"photo", "video", "no_thumb", "thumb_at", "ttl", "silent"}},
	{"传输参数", []string{"part_size", "concurrency", "retries", "no_resume",
		"state_dir", "ledger", "force", "connections"}},
	{"连接", []string{"session", "api_id", "api_hash", "proxy", "verbose"}},
}

// 这些是一次性动作，不允许出现在配置文件里
var neverFromConfig = map[string]bool{
	"from_config": true, "profile": true, "dump_config": true,
	"plan": true, "list_chats": true, "session_string": true,
}

// 需要强制转成字符串的键（YAML 会把 4Mbps 之外的东西解析成数字）
var strKeys = map[string]bool{
	"rate": true, "burst": true, "rest": true, "daily_cap": true,
	"min_size": true, "max_size": true, "cooldown": true, "min_rate": true,
	"ext": true, "to": true, "caption": true, "parse_mode": true,
	"sort": true, "on_cap": true, "session": true, "api_id": true,
	"api_hash": true, "proxy": true, "state_dir": true, "ledger": true,
	"thumb_at": true, "split_dir": true,
}
var listKeys = map[string]bool{"window": true, "inputs": true}
var pathKeys = map[string]bool{"inputs": true, "session": true, "ledger": true,
	"state_dir": true, "split_dir": true}

// dump 时给容易混淆量纲的键加行内提示
var fieldHint = map[string]string{
	"rate":      "速率，如 4Mbps / 500KB/s —— 唯一控制限速的字段",
	"burst":     "时长，如 20m —— 连续传多久，不是速率；须与 rest 成对出现",
	"rest":      "时长，如 10m —— 歇多久，不是速率",
	"cooldown":  "时长，如 15m —— 检测到被限速后的冷却时间",
	"min_rate":  "速率 —— 自适应降速的下限",
	"daily_cap": "大小，如 15GB",
	"min_size":  "大小，如 50MB",
	"max_size":  "大小",
}

func loadConfigFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("配置文件读取失败: %w", err)
	}
	text := strings.TrimPrefix(string(data), "\ufeff") // 容忍记事本写入的 BOM
	var raw map[string]any
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml", "":
		if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
			return nil, fmt.Errorf("%s 解析失败: %w", path, err)
		}
	case ".toml":
		if err := toml.Unmarshal([]byte(text), &raw); err != nil {
			return nil, fmt.Errorf("%s 解析失败: %w", path, err)
		}
	case ".json":
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return nil, fmt.Errorf("%s 解析失败: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("不认识的配置格式: %s（支持 yaml/toml/json）", filepath.Ext(path))
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

// closeMatch 返回 candidates 里与 target 最相近的一个（编辑距离足够小才有提示价值）。
func closeMatch(candidates []string, target string) string {
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		d := levenshtein(c, target)
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	// difflib 默认阈值约 0.6 相似度；这里用编辑距离 <= 一半长度 近似
	if best != "" && bestDist <= len(target)/2+1 {
		return best
	}
	return ""
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// sortedKeys 返回 map 的键（已排序），用于稳定的报错信息。
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// flattenConfig 允许一层层的分节写法，最终展平成 key -> value。未知键直接报错。
func flattenConfig(raw map[string]any, valid map[string]bool, where string) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range raw {
		key := strings.ReplaceAll(strings.TrimSpace(k), "-", "_")
		if valid[key] {
			out[key] = v
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			nested, err := flattenConfig(sub, valid, where+key+".")
			if err != nil {
				return nil, err
			}
			for k2, v2 := range nested {
				out[k2] = v2
			}
			continue
		}
		hint := closeMatch(sortedKeys(valid), key)
		extra := ""
		if hint != "" {
			extra = fmt.Sprintf("（是不是想写 %s？）", hint)
		}
		return nil, fmt.Errorf("配置项无法识别: %s%s%s", where, k, extra)
	}
	return out, nil
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func toList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s := toStr(item)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case nil:
		return nil
	default:
		return []string{toStr(v)}
	}
}

// toFloat 严格解析数值。
//
// 旧实现用 fmt.Sscanf(s, "%g", &f)：Sscanf 只要求「前缀能解析」，
// 于是 `gap_min: "12abc"` 会被静默读成 12，用户完全不知道自己写错了。
// strconv.ParseFloat 要求整串都是合法数字，该报错就报错。
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case int:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("不是数值: %v", v)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("不是数值: %v", v)
	}
}

func toInt(v any) (int64, error) {
	f, err := toFloat(v)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

func toBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(x) {
		case "true", "yes", "on", "1":
			return true, nil
		case "false", "no", "off", "0", "":
			return false, nil
		}
		return false, fmt.Errorf("不是布尔值: %q", x)
	case int, int64, float64:
		f, err := toFloat(v)
		if err != nil {
			return false, err
		}
		return f != 0, nil
	default:
		return false, fmt.Errorf("不是布尔值: %v", v)
	}
}

// coerceVal 对应 Python 的 _coerce：列表/字符串强制转换 + 相对路径解析。
func coerceVal(key string, v any, baseDir string) any {
	if listKeys[key] {
		v = toList(v)
	} else if strKeys[key] && v != nil {
		if _, isStr := v.(string); !isStr {
			v = toStr(v)
		}
	}
	if pathKeys[key] {
		switch x := v.(type) {
		case string:
			v = resolveAgainst(baseDir, x)
		case []string:
			out := make([]string, len(x))
			for i, p := range x {
				out[i] = resolveAgainst(baseDir, p)
			}
			v = out
		}
	}
	return v
}

func resolveAgainst(baseDir, p string) string {
	p = expandTilde(p)
	if filepath.IsAbs(p) {
		return p
	}
	abs, err := filepath.Abs(filepath.Join(baseDir, p))
	if err != nil {
		return p
	}
	return abs
}

// loadConfig 读取配置文件并叠加 profile，返回 键(下划线形式) -> 值。
func loadConfig(path, profile string) (map[string]any, error) {
	raw, err := loadConfigFile(path)
	if err != nil {
		return nil, err
	}
	profiles, _ := raw["profiles"].(map[string]any)
	delete(raw, "profiles")

	valid := map[string]bool{}
	for _, k := range allConfigKeys() {
		if !neverFromConfig[k] {
			valid[k] = true
		}
	}

	merged, err := flattenConfig(raw, valid, "")
	if err != nil {
		return nil, err
	}
	if profile != "" {
		p, ok := profiles[profile]
		if !ok {
			if names := sortedKeys(profiles); len(names) > 0 {
				return nil, fmt.Errorf("配置里没有 profile %q；可用: %s",
					profile, strings.Join(names, ", "))
			}
			return nil, fmt.Errorf("配置里没有 profile %q（该文件未定义 profiles 段）", profile)
		}
		pm, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("profiles.%s 必须是映射", profile)
		}
		nested, err := flattenConfig(pm, valid, "profiles."+profile+".")
		if err != nil {
			return nil, err
		}
		for k, v := range nested {
			merged[k] = v
		}
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		baseDir = "."
	}
	out := make(map[string]any, len(merged))
	for k, v := range merged {
		out[k] = coerceVal(k, v, baseDir)
	}
	if h, ok := out["api_hash"].(string); ok && h != "" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			logf("⚠ %s 对其他用户可读却含有 api_hash，建议 chmod 600", path)
		}
	}
	return out, nil
}

// allConfigKeys 返回全部配置键（下划线形式），供合法性检查与 dump 使用。
func allConfigKeys() []string {
	keys := []string{}
	for _, sec := range configSections {
		keys = append(keys, sec.Keys...)
	}
	return keys
}

func yamlQuote(s string) string {
	// 一律用 YAML 单引号：内部无转义语义，Windows 路径的反斜杠也不会被吃掉
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func emitYAML(key string, val any) []string {
	hint := ""
	if h, ok := fieldHint[key]; ok {
		hint = "  # " + h
	}
	switch x := val.(type) {
	case bool:
		return []string{fmt.Sprintf("%s: %t%s", key, x, hint)}
	case int, int64:
		if (key == "ttl" || key == "part_size") && fmt.Sprint(x) == "0" {
			return []string{fmt.Sprintf("# %s:%s", key, hint)} // Python 默认 None
		}
		return []string{fmt.Sprintf("%s: %v%s", key, x, hint)}
	case float32, float64:
		return []string{fmt.Sprintf("%s: %v%s", key, x, hint)}
	case []string:
		if len(x) == 0 {
			// Python 版里 window 默认 None、inputs 默认 []，排版保持一致
			if key == "window" {
				return []string{fmt.Sprintf("# %s:%s", key, hint)}
			}
			return []string{fmt.Sprintf("# %s: []%s", key, hint)}
		}
		out := []string{fmt.Sprintf("%s:%s", key, hint)}
		for _, item := range x {
			out = append(out, "  - "+yamlQuote(item))
		}
		return out
	case string:
		if x == "" {
			return []string{fmt.Sprintf("# %s:%s", key, hint)}
		}
		return []string{fmt.Sprintf("%s: %s%s", key, yamlQuote(x), hint)}
	default:
		return []string{fmt.Sprintf("# %s:%s", key, hint)}
	}
}

// dumpConfig 按当前参数输出一份带注释的 YAML 模板。
func dumpConfig(o *Options) string {
	out := []string{
		"# tgup 配置文件模板 —— 用 --from-config 加载",
		"# 注释掉的是未设置项；命令行显式参数会覆盖这里的值。",
		"# 相对路径按本文件所在目录解析。",
		"# 警告：本文件若含 api_id/api_hash，请 chmod 600 并确保不在版本控制中。",
		"",
	}
	for _, sec := range configSections {
		out = append(out, "# ---------------- "+sec.Title)
		for _, k := range sec.Keys {
			out = append(out, emitYAML(k, o.fieldFor(k))...)
		}
		out = append(out, "")
	}
	out = append(out,
		"# ---------------- 可选：命名档位，用 --profile <名字> 叠加",
		"# profiles:",
		"#   night:",
		"#     rate: 1.5Mbps",
		"#     window: [\"23:30-06:00\"]",
		"#     daily_cap: 5GB",
		"")
	return strings.Join(out, "\n")
}

// errConfigNotFound 供调用方区分「配置文件不存在」与其他解析错误。
var errConfigNotFound = errors.New("配置文件不存在")
