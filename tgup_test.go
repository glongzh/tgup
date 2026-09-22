package main

// 核心本地逻辑的单元测试：解析器、配置、台账、切割、实体、文件发现。
// 全部不依赖 Telegram 网络，`go test ./...` 即可跑。
//
// 需要真实视频的用例会现场用 ffmpeg 生成素材；环境里没有 ffmpeg 就跳过，
// 而不是像早先那样硬编码 /tmp/tgup-test-big.mp4——那在 Windows 上等于永远不跑。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"
)

// ---- 测试辅助 -----------------------------------------------------------

func writeFile(t *testing.T, path string, size int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return path
}

// makeTestVideo 生成一段带密集关键帧的测试视频，供切割用例使用。
// 用 mpeg4 而不是 libx264：前者是 ffmpeg 内置编码器，任何构建都有。
func makeTestVideo(t *testing.T, dir string, seconds int) string {
	t.Helper()
	if !hasTool("ffmpeg") {
		t.Skip("未安装 ffmpeg，跳过需要真实视频的用例")
	}
	out := filepath.Join(dir, "clip.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i",
		fmt.Sprintf("testsrc2=size=640x480:rate=30:duration=%d", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "mpeg4", "-q:v", "2", "-g", "5", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", out)
	if err := cmd.Run(); err != nil {
		t.Skipf("ffmpeg 生成测试视频失败（%v），跳过", err)
	}
	if fileSize(out) < 2*MB {
		t.Skipf("生成的测试视频只有 %d 字节，不足以测试切割", fileSize(out))
	}
	return out
}

func defaultOptions(t *testing.T) *Options {
	t.Helper()
	o := &Options{}
	if err := newFlagSet(o).Parse(nil); err != nil {
		t.Fatalf("解析默认参数失败: %v", err)
	}
	return o
}

// ---- 量纲解析 -----------------------------------------------------------

func TestParseRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"4Mbps", 500000},       // 4e6 bit/s = 500000 B/s（十进制前缀）
		{"500KB/s", 500 * 1024}, // 字节量用二进制前缀
		{"2MB/s", 2 * 1024 * 1024},
		{"1mbit", 125000},
		{"1B", 1},
	}
	for _, c := range cases {
		got, err := parseRate(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseRate(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	if _, err := parseRate("20m"); err == nil {
		t.Error("parseRate('20m') 应报错（无单位后缀）")
	}
}

// 回归：正则允许 "1.2.3" 这类输入通过，ParseFloat 会失败。
// 早先代码用 `_` 丢掉错误，会静默返回 0 —— 限速直接被关掉。
func TestParseRateRejectsMalformedNumber(t *testing.T) {
	for _, in := range []string{"1.2.3Mbps", ".Mbps", "1..2KB/s"} {
		if v, err := parseRate(in); err == nil {
			t.Errorf("parseRate(%q) 应报错，got %v", in, v)
		}
	}
}

func TestParseSizeDuration(t *testing.T) {
	if v, _ := parseSize("15GB"); v != 15*GB {
		t.Errorf("parseSize(15GB) = %d", v)
	}
	if v, _ := parseSize("500MiB"); v != 500*MB {
		t.Errorf("parseSize(500MiB) = %d", v)
	}
	if v, _ := parseDuration("20m"); v != 1200 {
		t.Errorf("parseDuration(20m) = %v", v)
	}
	if v, _ := parseDuration("2h"); v != 7200 {
		t.Errorf("parseDuration(2h) = %v", v)
	}
	if _, err := parseSize("1.2.3GB"); err == nil {
		t.Error("parseSize('1.2.3GB') 应报错")
	}
	if _, err := parseDuration("1.2.3m"); err == nil {
		t.Error("parseDuration('1.2.3m') 应报错")
	}
}

func TestParseWindow(t *testing.T) {
	w, err := parseWindow("09:00-23:30")
	if err != nil || w.Start != 9*60 || w.End != 23*60+30 {
		t.Fatalf("parseWindow = %+v, %v", w, err)
	}
	// 跨零点
	w, _ = parseWindow("23:30-06:00")
	if !inWindow(23*60+45, w) || !inWindow(5, w) || inWindow(12*60, w) {
		t.Errorf("跨零点窗口判断错误: %+v", w)
	}
	if _, err := parseWindow("25:00-26:00"); err != nil {
		// 小时数取模 24，不报错；这里只确认不会 panic
		_ = err
	}
	for _, bad := range []string{"09:60-10:00", "abc", "09:00", ""} {
		if _, err := parseWindow(bad); err == nil {
			t.Errorf("parseWindow(%q) 应报错", bad)
		}
	}
}

func TestParseThumbAt(t *testing.T) {
	cases := []struct {
		in        string
		ratio     bool
		value     float64
		wantError bool
	}{
		{"10%", true, 0.10, false},
		{"150%", true, 1.0, false}, // 夹到 100%
		{"90", false, 90, false},
		{"90s", false, 90, false},
		{"1.5m", false, 90, false},
		{"1h", false, 3600, false},
		{"", false, 0, false}, // 空串返回 nil
		{"abc", false, 0, true},
	}
	for _, c := range cases {
		got, err := parseThumbAt(c.in)
		if c.wantError {
			if err == nil {
				t.Errorf("parseThumbAt(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseThumbAt(%q) 报错: %v", c.in, err)
			continue
		}
		if c.in == "" {
			if got != nil {
				t.Errorf("parseThumbAt(\"\") 应为 nil")
			}
			continue
		}
		if got.Ratio != c.ratio || got.Value != c.value {
			t.Errorf("parseThumbAt(%q) = %+v, want ratio=%v value=%v", c.in, got, c.ratio, c.value)
		}
	}
}

func TestHuman(t *testing.T) {
	if got := human(78.8 * 1024); got != "78.8KB" {
		t.Errorf("human = %s", got)
	}
	if got := human(15 * GB); got != "15.0GB" {
		t.Errorf("human = %s", got)
	}
	if got := humanDur(3725); got != "1h02m" {
		t.Errorf("humanDur = %s", got)
	}
}

func TestCrossDetection(t *testing.T) {
	_, err := parseConfigured("rate", "20m")
	if err == nil || !strings.Contains(err.Error(), "看起来是时长") {
		t.Errorf("rate=20m 应提示量纲错误，got %v", err)
	}
	_, err = parseConfigured("burst", "4Mbps")
	if err == nil || !strings.Contains(err.Error(), "看起来是速率") {
		t.Errorf("burst=4Mbps 应提示量纲错误，got %v", err)
	}
}

// ---- 配置解析 -----------------------------------------------------------

// 回归：toFloat 曾用 fmt.Sscanf("%g")，"12abc" 会被静默读成 12。
func TestToFloatIsStrict(t *testing.T) {
	for _, bad := range []string{"12abc", "abc", "1.2.3", "--"} {
		if v, err := toFloat(bad); err == nil {
			t.Errorf("toFloat(%q) 应报错，got %v", bad, v)
		}
	}
	for _, ok := range []any{"12", " 12.5 ", 7, int64(3), 1.5} {
		if _, err := toFloat(ok); err != nil {
			t.Errorf("toFloat(%v) 不该报错: %v", ok, err)
		}
	}
}

func TestFlattenConfigNestedAndUnknown(t *testing.T) {
	valid := map[string]bool{"rate": true, "burst": true, "recursive": true}
	raw := map[string]any{
		"上行整形":  map[string]any{"rate": "4Mbps"},
		"burst": "20m",
	}
	got, err := flattenConfig(raw, valid, "")
	if err != nil {
		t.Fatalf("flattenConfig 失败: %v", err)
	}
	if got["rate"] != "4Mbps" || got["burst"] != "20m" {
		t.Errorf("展平结果错误: %v", got)
	}

	// 未知键要报错，并且给出拼写建议
	_, err = flattenConfig(map[string]any{"ratee": "4Mbps"}, valid, "")
	if err == nil {
		t.Fatal("未知键应报错")
	}
	if !strings.Contains(err.Error(), "是不是想写 rate") {
		t.Errorf("应给出拼写建议，got %v", err)
	}
}

func TestConfigCoerceRelativePaths(t *testing.T) {
	base := t.TempDir()
	v := coerceVal("state_dir", "sub/dir", base)
	if !filepath.IsAbs(v.(string)) {
		t.Errorf("相对路径未解析成绝对路径: %v", v)
	}
	if !strings.HasPrefix(v.(string), base) {
		t.Errorf("路径未按配置目录解析: %v", v)
	}
	// ~ 展开
	home, err := os.UserHomeDir()
	if err == nil {
		got := coerceVal("state_dir", "~/x", base).(string)
		if !strings.HasPrefix(got, home) {
			t.Errorf("~ 未展开: %v", got)
		}
	}
}

func TestLoadConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tgup.yaml")
	body := `
to: 'me'
rate: 4Mbps
burst: 20m
rest: 10m
window: ['09:00-23:30', '01:00-02:00']
recursive: true
profiles:
  night:
    rate: 1.5Mbps
    daily_cap: '5GB'
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path, "")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg["rate"] != "4Mbps" || cfg["recursive"] != true {
		t.Errorf("基础配置解析错误: %v", cfg)
	}
	if n := len(toList(cfg["window"])); n != 2 {
		t.Errorf("window 应为 2 项，got %d", n)
	}

	cfg, err = loadConfig(path, "night")
	if err != nil {
		t.Fatalf("loadConfig(night): %v", err)
	}
	if cfg["rate"] != "1.5Mbps" || cfg["daily_cap"] != "5GB" {
		t.Errorf("profile 叠加错误: %v", cfg)
	}

	if _, err := loadConfig(path, "nope"); err == nil {
		t.Error("不存在的 profile 应报错")
	}
}

func TestLoadConfigUnknownKeySuggests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("ratte: 4Mbps\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path, "")
	if err == nil || !strings.Contains(err.Error(), "是不是想写 rate") {
		t.Errorf("应提示拼写建议，got %v", err)
	}
}

func TestApplyConfigKeyRejectsUnknown(t *testing.T) {
	o := defaultOptions(t)
	if err := applyConfigKey(o, "no_such_key", 1); err == nil {
		t.Error("未知配置键应报错")
	}
	if err := applyConfigKey(o, "gap_min", "abc"); err == nil {
		t.Error("非法数值应报错")
	}
}

func TestCloseMatch(t *testing.T) {
	names := []string{"rate", "burst", "rest", "recursive"}
	if got := closeMatch(names, "ratte"); got != "rate" {
		t.Errorf("closeMatch(ratte) = %q", got)
	}
	if got := closeMatch(names, "zzzzzzzzzz"); got != "" {
		t.Errorf("毫不相干的键不该给建议，got %q", got)
	}
}

// ---- 参数校验 -----------------------------------------------------------

// 回归：retries=0 会让每个分片第一次尝试就判失败，整批文件静默跳过。
func TestValidateRejectsSilentFailures(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Options)
	}{
		{"retries=0", func(o *Options) { o.Retries = 0 }},
		{"concurrency=0", func(o *Options) { o.Concurrency = 0 }},
		{"connections=0", func(o *Options) { o.Connections = 0 }},
		{"connections 超上限", func(o *Options) { o.Connections = 99 }},
		{"sort 未知", func(o *Options) { o.Sort = "bogus" }},
		{"parse-mode 未知", func(o *Options) { o.ParseMode = "bogus" }},
		{"on-cap 未知", func(o *Options) { o.OnCap = "bogus" }},
		{"gap-max < gap-min", func(o *Options) { o.GapMin, o.GapMax = 100, 10 }},
		{"throttle-floor 越界", func(o *Options) { o.ThrottleFloor = 1.5 }},
		{"probe-window<=0", func(o *Options) { o.ProbeWindow = 0 }},
		{"ttl 为负", func(o *Options) { o.TTL = -1 }},
		{"part-size 越界", func(o *Options) { o.PartSize = MAX_PART_SIZE/KB + 1 }},
	}
	for _, c := range cases {
		o := defaultOptions(t)
		c.mut(o)
		if err := o.validate(); err == nil {
			t.Errorf("%s 应被校验拦下", c.name)
		}
	}
	if err := defaultOptions(t).validate(); err != nil {
		t.Errorf("默认参数不该报错: %v", err)
	}
}

func TestResolveArgsPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("rate: 4Mbps\nburst: 20m\nrest: 10m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 命令行显式给的 rate 应压过配置文件
	o, err := resolveArgs([]string{"--from-config", path, "--rate", "9Mbps"})
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	if o.Rate != "9Mbps" {
		t.Errorf("命令行未覆盖配置文件: rate=%q", o.Rate)
	}
	if o.Burst != "20m" {
		t.Errorf("配置文件的值未生效: burst=%q", o.Burst)
	}

	// --version 不需要配置文件，也不该因为缺凭据报错
	if o, err := resolveArgs([]string{"--version"}); err != nil || !o.ShowVersion {
		t.Errorf("--version 应短路: %v %v", o, err)
	}
}

// ---- caption / 实体 -----------------------------------------------------

// offsets 提取任意实体的 offset/length，返回 false 表示类型未知。
func offsets(e tg.MessageEntityClass) (int, int, bool) {
	switch v := e.(type) {
	case *tg.MessageEntityBold:
		return v.Offset, v.Length, true
	case *tg.MessageEntityItalic:
		return v.Offset, v.Length, true
	case *tg.MessageEntityCode:
		return v.Offset, v.Length, true
	case *tg.MessageEntityPre:
		return v.Offset, v.Length, true
	case *tg.MessageEntityTextURL:
		return v.Offset, v.Length, true
	case *tg.MessageEntityStrike:
		return v.Offset, v.Length, true
	case *tg.MessageEntitySpoiler:
		return v.Offset, v.Length, true
	case *tg.MessageEntityUnderline:
		return v.Offset, v.Length, true
	}
	return 0, 0, false
}

func entOffsets(t *testing.T, e tg.MessageEntityClass) (int, int) {
	t.Helper()
	off, ln, ok := offsets(e)
	if !ok {
		t.Fatalf("未知实体类型 %T", e)
	}
	return off, ln
}

func entityOf[T tg.MessageEntityClass](t *testing.T, ents []tg.MessageEntityClass, i int) T {
	t.Helper()
	if len(ents) <= i {
		t.Fatalf("实体数量不足: %d", len(ents))
	}
	e, ok := ents[i].(T)
	if !ok {
		t.Fatalf("ents[%d] 类型 %T 不符", i, ents[i])
	}
	return e
}

func TestMarkdownEntities(t *testing.T) {
	text, ents := markdownEntities("**加粗**普通`code`")
	if text != "加粗普通code" {
		t.Fatalf("text = %q", text)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %d", len(ents))
	}
	entityOf[*tg.MessageEntityBold](t, ents, 0)
	entityOf[*tg.MessageEntityCode](t, ents, 1)
}

func TestMarkdownPlainTextPassesThrough(t *testing.T) {
	// 没有任何标记时，文本必须原样返回（回归：曾经会因重复跑正则而错位）
	const s = "普通中文 caption with spaces & symbols 123"
	text, ents := markdownEntities(s)
	if text != s || len(ents) != 0 {
		t.Errorf("纯文本被改动: text=%q ents=%d", text, len(ents))
	}
}

func TestMarkdownUTF16Offsets(t *testing.T) {
	// BMP 字符（含 CJK）每个占 1 个 UTF-16 单元
	text, ents := markdownEntities("**中文**ab")
	if text != "中文ab" || len(ents) != 1 {
		t.Fatalf("text=%q ents=%d", text, len(ents))
	}
	if off, ln := entOffsets(t, ents[0]); off != 0 || ln != 2 {
		t.Errorf("offset/length = %d/%d, want 0/2", off, ln)
	}
	text, ents = markdownEntities("前缀**中文**")
	if text != "前缀中文" || len(ents) != 1 {
		t.Fatalf("text=%q ents=%d", text, len(ents))
	}
	if off, ln := entOffsets(t, ents[0]); off != 2 || ln != 2 {
		t.Errorf("offset/length = %d/%d, want 2/2", off, ln)
	}
	// 星界字符（emoji）占 2 个 UTF-16 单元
	text, ents = markdownEntities("**😀**x")
	if text != "😀x" || len(ents) != 1 {
		t.Fatalf("text=%q ents=%d", text, len(ents))
	}
	if off, ln := entOffsets(t, ents[0]); off != 0 || ln != 2 {
		t.Errorf("offset/length = %d/%d, want 0/2", off, ln)
	}
}

// 回归：``` ``` 块允许空正文，会产出 length=0 的实体，Telegram 直接拒收整条消息。
// 由模糊测试输入 "``````" 命中。
// 回归：非法 UTF-8 字节会在片段拼接处重新组合成合法序列
// （`\xd2` 与 `\x87` 分开看都是坏字节，拼起来是 U+0487），
// 导致「各片段 UTF-16 长度之和」大于整体长度，算出的实体越界。
// 由模糊测试输入 "000000\xd2**\x87**" 命中。
func TestMarkdownInvalidUTF8DoesNotBreakOffsets(t *testing.T) {
	inputs := []string{
		"000000\xd2**\x87**",
		"\xd2**x**",
		"**\x87**",
		"\xff\xfe\xfd",
		"中文\xc3**中文**", // 截断的多字节序列
	}
	for _, in := range inputs {
		text, ents := markdownEntities(in)
		limit := utf16Len(text)
		if !utf8.ValidString(text) {
			t.Errorf("输出应被净化为合法 UTF-8: %q", text)
		}
		for i, e := range ents {
			off, ln, ok := offsets(e)
			if !ok {
				t.Fatalf("未知实体类型 %T", e)
			}
			if off < 0 || ln <= 0 || off+ln > limit {
				t.Errorf("输入 %q：实体 %d 越界 off=%d ln=%d limit=%d（text=%q）",
					in, i, off, ln, limit, text)
			}
		}
	}
}

func TestMarkdownEmptyPreProducesNoEntity(t *testing.T) {
	for _, in := range []string{"``````", "```go\n```", "```\n```"} {
		text, ents := markdownEntities(in)
		if len(ents) != 0 {
			t.Errorf("markdownEntities(%q) 不该产生实体，got %d 个（text=%q）", in, len(ents), text)
		}
	}
}

func TestMarkdownLinkAndPre(t *testing.T) {
	text, ents := markdownEntities("看 [这里](https://example.com)")
	if text != "看 这里" || len(ents) != 1 {
		t.Fatalf("text=%q ents=%d", text, len(ents))
	}
	u := entityOf[*tg.MessageEntityTextURL](t, ents, 0)
	if u.URL != "https://example.com" || u.Offset != 2 || u.Length != 2 {
		t.Errorf("链接实体错误: %+v", u)
	}

	text, ents = markdownEntities("```go\nfmt.Println()\n```")
	if len(ents) != 1 {
		t.Fatalf("ents=%d", len(ents))
	}
	p := entityOf[*tg.MessageEntityPre](t, ents, 0)
	if p.Language != "go" || p.Length != utf16Len(text) {
		t.Errorf("预格式实体错误: %+v text=%q", p, text)
	}
}

func TestHTMLEntities(t *testing.T) {
	text, ents := htmlEntities("a&lt;b<i>斜</i>尾")
	if text != "a<b斜尾" || len(ents) != 1 {
		t.Fatalf("text=%q ents=%d", text, len(ents))
	}
	if off, ln := entOffsets(t, ents[0]); off != 3 || ln != 1 {
		t.Errorf("offset/length = %d/%d, want 3/1", off, ln)
	}
	_, ents = htmlEntities(`<a href="https://example.com">链接</a>`)
	if len(ents) != 1 {
		t.Fatalf("ents=%d", len(ents))
	}
	if u, ok := ents[0].(*tg.MessageEntityTextURL); !ok || u.URL != "https://example.com" {
		t.Errorf("链接实体错误: %T %+v", ents[0], ents[0])
	}
}

func TestHTMLEntitiesUnclosedAndSingleQuotes(t *testing.T) {
	// 未闭合标签按到文末处理，不能 panic，也不能产生越界实体
	text, ents := htmlEntities("<b>没有闭合")
	if text != "没有闭合" || len(ents) != 1 {
		t.Fatalf("text=%q ents=%d", text, len(ents))
	}
	if off, ln := entOffsets(t, ents[0]); off != 0 || ln != utf16Len(text) {
		t.Errorf("offset/length = %d/%d", off, ln)
	}
	// 单引号属性
	_, ents = htmlEntities(`<a href='https://x.dev'>x</a>`)
	if len(ents) != 1 {
		t.Fatalf("ents=%d", len(ents))
	}
	if u := entityOf[*tg.MessageEntityTextURL](t, ents, 0); u.URL != "https://x.dev" {
		t.Errorf("单引号 href 解析失败: %q", u.URL)
	}
}

func TestBuildCaptionNone(t *testing.T) {
	text, ents := buildCaption("hello", "none")
	if text != "hello" || ents != nil {
		t.Errorf("none 模式不应产生实体")
	}
	if text, ents := buildCaption("", "md"); text != "" || ents != nil {
		t.Errorf("空文本不应产生实体")
	}
}

// ---- 切割 ---------------------------------------------------------------

func TestSplitFileAndReuse(t *testing.T) {
	src := makeTestVideo(t, t.TempDir(), 10)
	dir := t.TempDir()
	const maxBytes = 2 * MB

	parts, err := splitFile(context.Background(), src, maxBytes, dir)
	if err != nil {
		t.Fatalf("splitFile: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("应切成多段，got %d", len(parts))
	}
	for _, p := range parts {
		if fileSize(p) > maxBytes {
			t.Errorf("分片超限: %s = %d", p, fileSize(p))
		}
	}
	// 分片必须完整，且段数守恒（防止重命名环节丢文件）
	var sum int64
	for _, p := range parts {
		sum += fileSize(p)
	}
	if sum == 0 {
		t.Error("分片全为空")
	}

	// manifest 复用
	parts2, err := splitFile(context.Background(), src, maxBytes, dir)
	if err != nil {
		t.Fatalf("splitFile reuse: %v", err)
	}
	if len(parts2) != len(parts) {
		t.Errorf("复用段数不一致: %d vs %d", len(parts2), len(parts))
	}

	// 清理
	root := filepath.Dir(parts[0])
	cleanupSplit(root)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("分片目录未清理")
	}
}

func TestSplitFileDefaultDir(t *testing.T) {
	// 回归：未设置 --split-dir 时，分片目录应建在源文件「旁边」，
	// 而不是把源文件当父目录（单文件 inputs + 超限切割的路径）。
	src := makeTestVideo(t, t.TempDir(), 10)
	dir := t.TempDir()
	local := filepath.Join(dir, "测试.mp4")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取测试视频失败: %v", err)
	}
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("复制测试视频失败: %v", err)
	}

	parts, err := splitFile(context.Background(), local, 2*MB, "") // splitDir 为空 → 默认放源文件旁边
	if err != nil {
		t.Fatalf("splitFile(默认目录): %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("应切成多段，got %d", len(parts))
	}
	wantRoot := filepath.Join(dir, SPLIT_DIR_PREFIX+"测试")
	if _, err := os.Stat(wantRoot); err != nil {
		t.Errorf("分片目录 %s 未创建: %v", wantRoot, err)
	}
	for _, p := range parts {
		if filepath.Dir(p) != wantRoot {
			t.Errorf("分片不在预期目录: %s", p)
		}
		if fileSize(p) > 2*MB {
			t.Errorf("分片超限: %s = %d", p, fileSize(p))
		}
	}
	// manifest 复用同样走默认目录
	parts2, err := splitFile(context.Background(), local, 2*MB, "")
	if err != nil || len(parts2) != len(parts) {
		t.Errorf("默认目录 manifest 复用失败: %v", err)
	}
}

// 回归：源文件改动后旧分片必须作废，否则会把过期的内容当成新的传上去。
func TestSplitManifestInvalidatedOnSourceChange(t *testing.T) {
	src := makeTestVideo(t, t.TempDir(), 10)
	dir := t.TempDir()
	parts, err := splitFile(context.Background(), src, 2*MB, dir)
	if err != nil {
		t.Fatalf("splitFile: %v", err)
	}
	root := filepath.Dir(parts[0])
	// 伪造一份 mtime 不匹配的清单
	manifest := filepath.Join(root, SPLIT_MANIFEST)
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m splitManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	// 磁盘上的清单声明 mtime=X+1，而调用方拿的是源文件真实的 mtime=X
	origMtime := m.Mtime
	m.Mtime = origMtime + 1
	bumped, _ := json.Marshal(m)
	if err := os.WriteFile(manifest, bumped, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadManifest(root, src, m.Size, origMtime); got != nil {
		t.Errorf("源文件变过后不该复用旧分片，got %v", got)
	}
}

// ---- 台账与续传 ---------------------------------------------------------

func TestLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l := openLedger(path)
	l.Mark("/tmp/a.mp4", 123, 456, "me", 42)
	l.AddToday(1000)
	l.Flush()

	l2 := openLedger(path)
	if !l2.IsDone("/tmp/a.mp4", 123, 456, "me") {
		t.Error("台账应命中")
	}
	if l2.IsDone("/tmp/a.mp4", 123, 456, "other") {
		t.Error("不同目标不应命中")
	}
	if l2.UsedToday() != 1000 {
		t.Errorf("UsedToday = %d", l2.UsedToday())
	}
}

// 台账存着本机绝对路径与消息 id，不该对其他用户可读。
func TestLedgerPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不体现 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "ledger.json")
	l := openLedger(path)
	l.Mark("/tmp/a.mp4", 1, 2, "me", 3)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("台账权限过宽: %v", fi.Mode().Perm())
	}
}

// 回归：临时文件名不能等于目标名，否则 rename 变成自己改自己，原子性失效。
func TestWriteFileAtomicNoExtCollision(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ledger.json", "ledger"} {
		path := filepath.Join(dir, name)
		if err := writeFileAtomic(path, []byte("hello")); err != nil {
			t.Fatalf("writeFileAtomic(%s): %v", name, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "hello" {
			t.Errorf("读回失败 %s: %q %v", name, got, err)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("临时文件未清理: %s.tmp", name)
		}
	}
}

// 续传状态的磁盘格式必须与 Python 版逐字节一致，否则两个版本无法互换续传。
func TestUploadStateFileFormat(t *testing.T) {
	dir := t.TempDir()
	st := &uploadState{
		path:       filepath.Join(dir, "s.json"),
		fileID:     1,
		partSize:   512,
		totalParts: 10,
		done:       map[int]struct{}{0: {}, 2: {}},
		created:    1700000000,
	}
	st.flush()
	data, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"file_id":1,"part_size":512,"total_parts":10,"done":[0,2],"created":1700000000}`
	if string(data) != want {
		t.Errorf("状态文件格式变了（会影响与 Python 版互换）:\n got %s\nwant %s", data, want)
	}
}

func TestUploadStateTTL(t *testing.T) {
	dir := t.TempDir()
	st := loadOrNewState(dir, "/tmp/b.mp4", 512*KB, 10, 5*MB, 1)
	st.markDone(0)
	st.markDone(1)
	st.flush()

	st2 := loadOrNewState(dir, "/tmp/b.mp4", 512*KB, 10, 5*MB, 1)
	if st2.doneCount() != 2 || st2.fileID != st.fileID {
		t.Errorf("续传状态未恢复: done=%d fileID=%d", st2.doneCount(), st2.fileID)
	}
	// part_size 不一致 → 重新开始
	st3 := loadOrNewState(dir, "/tmp/b.mp4", 256*KB, 10, 5*MB, 1)
	if st3.doneCount() != 0 {
		t.Error("分片参数变化应重置状态")
	}
}

func TestRandFileIDRange(t *testing.T) {
	for i := 0; i < 100; i++ {
		v := randFileID()
		if v < -(1<<62) || v > (1<<62)-1 {
			t.Fatalf("randFileID 越界: %d", v)
		}
	}
}

// ---- 分片大小 -----------------------------------------------------------

func TestPickPartSize(t *testing.T) {
	// 1GB / 4000 片：2048 片 @512KB
	ps, err := pickPartSize(1*GB, 4000)
	if err != nil || ps != 512*KB {
		t.Errorf("pickPartSize(1GB) = %d, %v", ps, err)
	}
	// 2^31 字节 / 4000 片 = 4096 片 > 4000，与 Python 版一致地报错
	//（Telegram 的 "2GB" 上限实际是 2e9 字节，4000×512KB=2.097e3 MB 是真实硬顶）
	if _, err := pickPartSize(2*GB, 4000); err == nil {
		t.Error("2*2^30 字节应超出 4000 片上限")
	}
	// 8000×512KB = 4194304000 恰好达标（premium）
	ps, err = pickPartSize(8000*512*KB, 8000)
	if err != nil || ps != 512*KB {
		t.Errorf("pickPartSize(8000×512KB) = %d, %v", ps, err)
	}
	if _, err := pickPartSize(10*GB, 4000); err == nil {
		t.Error("超上限应报错")
	}
}

func TestValidatePartSize(t *testing.T) {
	for _, ok := range []int{128 * KB, 256 * KB, 512 * KB} {
		if err := validatePartSize(ok); err != nil {
			t.Errorf("validatePartSize(%d) 不该报错: %v", ok, err)
		}
	}
	for _, bad := range []int{0, 3 * KB, 100 * KB, 1024 * KB, -1} {
		if err := validatePartSize(bad); err == nil {
			t.Errorf("validatePartSize(%d) 应报错", bad)
		}
	}
}

// ---- 文件发现 -----------------------------------------------------------

// 回归：递归扫描时 WalkDir 回调返回 nil 并不会跳过目录，
// 隐藏目录（.git / .cache）会被整个扫进来。
func TestDiscoverSkipsHiddenAndSplitDirs(t *testing.T) {
	dir := t.TempDir()
	want := []string{
		writeFile(t, filepath.Join(dir, "a.mp4"), 100),
		writeFile(t, filepath.Join(dir, "sub", "d.mp4"), 100),
	}
	// 这些都不该被发现
	writeFile(t, filepath.Join(dir, ".hidden", "b.mp4"), 100)
	writeFile(t, filepath.Join(dir, SPLIT_DIR_PREFIX+"a", "c.mp4"), 100)
	writeFile(t, filepath.Join(dir, ".dotfile.mp4"), 100)

	got := discover([]string{dir}, true, map[string]struct{}{"mp4": {}}, 0, 0, "name", false)
	if len(got) != len(want) {
		t.Fatalf("扫描结果数量不符: got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("扫描结果[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestDiscoverExplicitFileBypassesExtFilter(t *testing.T) {
	dir := t.TempDir()
	odd := writeFile(t, filepath.Join(dir, "movie.rmvb"), 100)
	writeFile(t, filepath.Join(dir, "notes.txt"), 100)

	// 目录扫描时 .rmvb 不在白名单里，扫不到
	exts := map[string]struct{}{"mp4": {}}
	if got := discover([]string{dir}, false, exts, 0, 0, "name", false); len(got) != 0 {
		t.Errorf("目录扫描不该命中非白名单扩展名: %v", got)
	}
	// 直接点名则不受白名单约束
	got := discover([]string{odd}, false, exts, 0, 0, "name", false)
	if len(got) != 1 || got[0] != odd {
		t.Errorf("显式指定文件应被接受: %v", got)
	}
}

func TestDiscoverSortDeterministic(t *testing.T) {
	dir := t.TempDir()
	for i, n := range []int{30, 10, 20} {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d.mp4", i)), n)
	}
	exts := map[string]struct{}{"mp4": {}}

	asc := discover([]string{dir}, false, exts, 0, 0, "size", false)
	if len(asc) != 3 || fileSize(asc[0]) != 10 || fileSize(asc[2]) != 30 {
		t.Errorf("size 升序错误: %v", asc)
	}
	desc := discover([]string{dir}, false, exts, 0, 0, "size", true)
	if len(desc) != 3 || fileSize(desc[0]) != 30 || fileSize(desc[2]) != 10 {
		t.Errorf("size 降序错误: %v", desc)
	}
}

// 回归：比较函数写成 `return !less`，等值时返回 true，
// 既违反严格弱序，又让 --reverse 下的顺序在多次运行间随机。
func TestDiscoverEqualKeysKeepPathOrder(t *testing.T) {
	dir := t.TempDir()
	names := []string{"c.mp4", "a.mp4", "b.mp4"}
	for _, n := range names {
		writeFile(t, filepath.Join(dir, n), 500) // 大小完全相同
	}
	exts := map[string]struct{}{"mp4": {}}

	for _, reverse := range []bool{false, true} {
		first := discover([]string{dir}, false, exts, 0, 0, "size", reverse)
		for i := 0; i < 20; i++ {
			again := discover([]string{dir}, false, exts, 0, 0, "size", reverse)
			for j := range first {
				if again[j] != first[j] {
					t.Fatalf("reverse=%v 时顺序不稳定:\n%v\n%v", reverse, first, again)
				}
			}
		}
		// 等值项按路径字典序排列
		for i := 1; i < len(first); i++ {
			if first[i-1] > first[i] {
				t.Errorf("等值项未按路径排序: %v", first)
				break
			}
		}
	}
}

func TestDiscoverSizeFilter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "small.mp4"), 10)
	big := writeFile(t, filepath.Join(dir, "big.mp4"), 5000)
	exts := map[string]struct{}{"mp4": {}}

	got := discover([]string{dir}, false, exts, 100, 0, "name", false)
	if len(got) != 1 || got[0] != big {
		t.Errorf("min-size 过滤失败: %v", got)
	}
	got = discover([]string{dir}, false, exts, 0, 100, "name", false)
	if len(got) != 1 || got[0] == big {
		t.Errorf("max-size 过滤失败: %v", got)
	}
}

// ---- 限速器 -------------------------------------------------------------

func TestPacerTokenBucket(t *testing.T) {
	// 空桶起步：限速 200KB/s 传 600KB 应耗时 ≈3s（600KB/200KB/s），而不是全速
	p := newPacer(200*KB, 0, 0, nil, 0, nil, "stop", false, 0.4, 180, 900, 0)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := p.Acquire(context.Background(), 200*KB); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	el := time.Since(start).Seconds()
	if el < 2.5 || el > 4.5 {
		t.Errorf("令牌桶限速未生效: 耗时 %.2fs（期望 ≈3s）", el)
	}
	if p.SentBytes() != 600*KB {
		t.Errorf("SentBytes = %d", p.SentBytes())
	}
}

func TestPacerDailyCap(t *testing.T) {
	dir := t.TempDir()
	lg := openLedger(filepath.Join(dir, "ledger.json"))
	p := newPacer(0, 0, 0, nil, 1000, lg, "stop", false, 0.4, 180, 900, 0)
	lg.AddToday(900)
	if err := p.Acquire(context.Background(), 200); err == nil {
		t.Error("超日配额应报错")
	} else if _, ok := err.(*dailyCapError); !ok {
		t.Errorf("应返回 dailyCapError，got %T", err)
	}
}

func TestPacerCancellation(t *testing.T) {
	p := newPacer(10*KB, 0, 0, nil, 0, nil, "stop", false, 0.4, 180, 900, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	// 限速极低，必然要等待；ctx 到期后应立即返回而不是睡满
	err := p.Acquire(ctx, 100*KB)
	if err == nil {
		t.Fatal("应因 ctx 取消而返回错误")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("取消后返回过慢: %v", el)
	}
}

func TestPacerWindow(t *testing.T) {
	// 构造一个必然包含「当前时刻」的窗口，Acquire 不该阻塞
	now := time.Now()
	startMin := now.Hour()*60 + now.Minute()
	w := TimeWindow{Start: startMin, End: (startMin + 60) % (24 * 60)}
	p := newPacer(0, 0, 0, []TimeWindow{w}, 0, nil, "stop", false, 0.4, 180, 900, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Acquire(ctx, 1); err != nil {
		t.Errorf("处于允许时段内不该阻塞: %v", err)
	}
}

func TestPacerDutyCycleJitter(t *testing.T) {
	for i := 0; i < 100; i++ {
		v := jitterValue(100, 0.15)
		if v < 85 || v > 115 {
			t.Fatalf("抖动超出 ±15%%: %v", v)
		}
	}
}

// ---- 进度条 -------------------------------------------------------------

func TestProgressLine(t *testing.T) {
	p := newProgress("测试视频.mp4", 1000, 500, "[1/2] ")
	line := p.line(100, 50.0, 5)
	if !strings.Contains(line, "50.0%") || !strings.Contains(line, "500B/1000B") || !strings.Contains(line, "测试视频.mp4") {
		t.Errorf("进度行异常: %q", line)
	}
}

func TestFitDisplay(t *testing.T) {
	if got := fitDisplay("中文abc", 20); dispWidth(got) != 20 {
		t.Errorf("补齐宽度错误: %q width=%d", got, dispWidth(got))
	}
	got := fitDisplay("这是一个很长的中文标题需要被截断处理", 10)
	if dispWidth(got) != 10 {
		t.Errorf("截断宽度错误: %q width=%d", got, dispWidth(got))
	}
	if !strings.HasSuffix(strings.TrimRight(got, " "), "…") {
		t.Errorf("截断应以省略号收尾: %q", got)
	}
	if got := fitDisplay("中文中文a很长的标题需要被截断处理", 10); dispWidth(got) != 10 {
		t.Errorf("混合宽度截断宽度错误: %q width=%d", got, dispWidth(got))
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("取不到家目录")
	}
	if got := expandTilde("~"); got != home {
		t.Errorf("expandTilde(~) = %q", got)
	}
	if got := expandTilde("~/a/b"); got != filepath.Join(home, "a/b") {
		t.Errorf("expandTilde(~/a/b) = %q", got)
	}
	if got := expandTilde("/abs/path"); got != "/abs/path" {
		t.Errorf("绝对路径不该被改动: %q", got)
	}
}

// ---- fuzz ---------------------------------------------------------------

// 量纲解析器是全项目最容易被奇怪输入搞崩的地方：它们直接吃用户手打的字符串。
func FuzzParseUnits(f *testing.F) {
	for _, s := range []string{
		"4Mbps", "500KB/s", "20m", "1.5h", "15GB", "500MiB", "09:00-23:30",
		"10%", "90s", "", ".", "-1", "1e999", "0x10", "99999999999999999999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseRate(s)
		_, _ = parseSize(s)
		_, _ = parseDuration(s)
		_, _ = parseThumbAt(s)
		_, _ = parseWindow(s)
		_, _ = toFloat(s)
		_, _ = toBool(s)
		_ = closeMatch([]string{"rate", "burst"}, s)
	})
}

// caption 解析出的实体，offset/length 必须落在纯文本的 UTF-16 范围内。
// 越界会被 Telegram 直接拒收，而这个错误在本地很难肉眼发现。
func FuzzCaptionEntities(f *testing.F) {
	for _, s := range []string{
		"**粗体**", "__斜__", "~~删~~", "||剧透||", "`code`",
		"```go\nx\n```", "[文字](https://a.b)", "😀**中文**x",
		"<b>x</b>", "<a href='u'>y</a>", "<i>未闭合", "&lt;&amp;&gt;",
		"***", "[[[", "```", "|||", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for name, fn := range map[string]func(string) (string, []tg.MessageEntityClass){
			"markdown": markdownEntities,
			"html":     htmlEntities,
		} {
			text, ents := fn(s)
			limit := utf16Len(text)
			for i, e := range ents {
				off, ln, ok := offsets(e)
				if !ok {
					t.Fatalf("%s: 未知实体类型 %T", name, e)
				}
				if off < 0 || ln <= 0 || off+ln > limit {
					t.Fatalf("%s: 实体 %d 越界 off=%d ln=%d limit=%d\n输入=%q\n文本=%q",
						name, i, off, ln, limit, s, text)
				}
			}
		}
	})
}

// 配置解析不能因为畸形输入 panic（尤其是嵌套与类型错配）。
func FuzzFlattenConfig(f *testing.F) {
	f.Add("rate", "4Mbps")
	f.Add("sub", "x")
	f.Fuzz(func(t *testing.T, k, v string) {
		valid := map[string]bool{"rate": true, "burst": true}
		raw := map[string]any{k: v}
		if strings.HasPrefix(v, "{") {
			raw[k] = map[string]any{"rate": v}
		}
		_, _ = flattenConfig(raw, valid, "")
	})
}
