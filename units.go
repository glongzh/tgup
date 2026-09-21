package main

// 单位换算与人类可读格式化、各种量纲的解析器。
// 对应 tgup.py 的常量与 parse_rate / parse_size / parse_duration / parse_window 一节。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

const (
	KB = 1024
	MB = 1024 * KB
	GB = 1024 * MB
	TB = 1024 * GB

	BIG_FILE_THRESHOLD = 10 * MB
	MAX_PARTS          = 4000
	MAX_PARTS_PREMIUM  = 8000
	MAX_PART_SIZE      = 512 * KB
	MIN_PART_SIZE      = 128 * KB

	// 上传连接池上限。--help 里一直写着「上限 8」，但代码里从来没有真正拦住过，
	// 现在由 Options.validate() 强制。
	MAX_CONNECTIONS = 8

	// 续传状态的有效期。服务端替未完成的上传保管分片能保管多久没有任何文档保证，
	// 经验值是几小时量级——过期后本地状态仍然记着"这 2800 片都传过了"，于是跳过
	// 它们、直传 send_media，服务端才回一句 FILE_PART_X_MISSING，整份白传。
	// 所以宁可保守：超过这个岁数的状态一律作废重传（真过期了 send_one 里也有兜底）。
	STATE_TTL = 6 * 3600

	HOME_DIR          = "~/.tgup"
	DEFAULT_SESSION   = "~/.tgup/tgup-go.session"
	DEFAULT_STATE_DIR = "~/.tgup/state"
	DEFAULT_LEDGER    = "~/.tgup/ledger.json"

	VIDEO_EXTS = "mp4,mkv,mov,avi,webm,flv,ts,m2ts,m4v,wmv,mpg,mpeg,rmvb,3gp,vob"

	// 超限切割相关（详见 split.go）
	SPLIT_SAFETY     = 0.95
	SPLIT_DIR_PREFIX = ".tgup-split-"
	SPLIT_MANIFEST   = "_split.json"
	MAX_SPLIT_DEPTH  = 4
)

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func human(n float64) string {
	if absF(n) < 1024 {
		return fmt.Sprintf("%dB", int(n))
	}
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		n /= 1024
		if absF(n) < 1024 || unit == "TB" {
			return fmt.Sprintf("%.1f%s", n, unit)
		}
	}
	return fmt.Sprintf("%.1fTB", n)
}

func humanDur(s float64) string {
	si := int(s)
	if si < 0 {
		si = 0
	}
	switch {
	case si < 60:
		return fmt.Sprintf("%ds", si)
	case si < 3600:
		return fmt.Sprintf("%dm%02ds", si/60, si%60)
	default:
		return fmt.Sprintf("%dh%02dm", si/3600, (si%3600)/60)
	}
}

// TimeWindow 以「当天第几分钟」表示时间点，规避时区/夏令时换算。
type TimeWindow struct{ Start, End int }

var rateRe = regexp.MustCompile(`^([\d.]+)\s*([KMGkmg]?)(bps|bit|b|B/s|Bps|B)$`)
var sizeRe = regexp.MustCompile(`^\s*([\d.]+)\s*([KMGTkmgt]?)i?[Bb]?\s*$`)
var durRe = regexp.MustCompile(`^\s*([\d.]+)\s*([smhSMH]?)\s*$`)
var winRe = regexp.MustCompile(`^\s*(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})\s*$`)
var thumbRe = regexp.MustCompile(`^([\d.]+)\s*([smhSMH]?)$`)

// parseRate: '4Mbps' / '4mbit' -> 比特每秒换算成字节; '500KB/s' / '500KBps' -> 字节每秒。
// 小写 b = bit，大写 B = Byte。单位后缀必须写，否则 '20m' 这种本意是时长的值
// 会被静默当成速率解析，而不是报错。
func parseRate(text string) (float64, error) {
	m := rateRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0, fmt.Errorf("无法解析速率: %q （例: 4Mbps / 500KB/s / 2MB/s）", text)
	}
	// 正则只保证 m[1] 由数字和小数点组成，但 "1.2.3" / "." 这类仍然过不了
	// ParseFloat，必须检查错误——早先用 `_` 丢掉，会静默当成 0 继续跑。
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析速率: %q （例: 4Mbps / 500KB/s / 2MB/s）", text)
	}
	isBit := m[3] == "bps" || m[3] == "bit" || m[3] == "b"
	// 比特率沿用网络惯例的十进制前缀（Mbps = 10^6 bit/s）；
	// 字节量沿用存储惯例的二进制前缀（MB = 2^20 byte）。
	base := 1024.0
	if isBit {
		base = 1000.0
	}
	switch strings.ToLower(m[2]) {
	case "k":
		val *= base
	case "m":
		val *= base * base
	case "g":
		val *= base * base * base
	}
	if isBit {
		val /= 8
	}
	return val, nil
}

func parseSize(text string) (int64, error) {
	m := sizeRe.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("无法解析大小: %q （例: 15GB / 500MB）", text)
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析大小: %q （例: 15GB / 500MB）", text)
	}
	var mult float64
	switch strings.ToLower(m[2]) {
	case "":
		mult = 1
	case "k":
		mult = KB
	case "m":
		mult = MB
	case "g":
		mult = GB
	case "t":
		mult = TB
	}
	return int64(val * mult), nil
}

// parseDuration: '20m' / '90s' / '2h'。裸数字按「分钟」解释。
func parseDuration(text string) (float64, error) {
	m := durRe.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("无法解析时长: %q （例: 20m / 90s / 2h）", text)
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析时长: %q （例: 20m / 90s / 2h）", text)
	}
	var mult float64
	switch strings.ToLower(m[2]) {
	case "", "m":
		mult = 60
	case "s":
		mult = 1
	case "h":
		mult = 3600
	}
	return val * mult, nil
}

func parseWindow(text string) (TimeWindow, error) {
	m := winRe.FindStringSubmatch(text)
	if m == nil {
		return TimeWindow{}, fmt.Errorf("无法解析时间窗: %q （例: 09:00-23:30）", text)
	}
	var g [4]int
	for i := 0; i < 4; i++ {
		g[i], _ = strconv.Atoi(m[i+1])
	}
	if g[1] >= 60 || g[3] >= 60 {
		return TimeWindow{}, fmt.Errorf("无法解析时间窗: %q （例: 09:00-23:30）", text)
	}
	return TimeWindow{Start: (g[0]%24)*60 + g[1], End: (g[2]%24)*60 + g[3]}, nil
}

func inWindow(minuteOfDay int, w TimeWindow) bool {
	if w.Start <= w.End {
		return w.Start <= minuteOfDay && minuteOfDay < w.End
	}
	return minuteOfDay >= w.Start || minuteOfDay < w.End // 跨零点
}

// ThumbAt 描述 --thumb-at：Ratio=true 时 Value 是时长比例，否则是绝对秒数。
type ThumbAt struct {
	Ratio bool
	Value float64
}

// parseThumbAt: '10%' -> 按时长比例；'90' / '90s' / '1.5m' -> 绝对秒数。
func parseThumbAt(text string) (*ThumbAt, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil, nil
	}
	if strings.HasSuffix(t, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(t, "%"), 64)
		if err != nil {
			return nil, fmt.Errorf("无法解析 --thumb-at: %q（例: 10%% / 90s / 1.5m）", text)
		}
		v = max(0, min(100, v))
		return &ThumbAt{Ratio: true, Value: v / 100}, nil
	}
	m := thumbRe.FindStringSubmatch(t)
	if m == nil {
		return nil, fmt.Errorf("无法解析 --thumb-at: %q（例: 10%% / 90s / 1.5m）", text)
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	var mult float64
	switch strings.ToLower(m[2]) {
	case "", "s":
		mult = 1
	case "m":
		mult = 60
	case "h":
		mult = 3600
	}
	// 裸数字按「秒」解释（和 --burst 的「分钟」不同，那是时段、这是时间点）
	return &ThumbAt{Value: v * mult}, nil
}

// ---- 量纲交叉检测 -------------------------------------------------------
//
// 每个配置字段该填哪种量纲。burst/rest/cooldown 是"传多久、歇多久"的时长，
// 不是速率——真正限速只有 rate 一个字段。用户猜错字段时直接指出，而不是裸报错。

var fieldKind = map[string]string{
	"rate": "速率", "min_rate": "速率",
	"burst": "时长", "rest": "时长", "cooldown": "时长",
	"daily_cap": "大小", "min_size": "大小", "max_size": "大小",
}
var kindParser = map[string]func(string) (float64, error){
	"速率": parseRate,
	"时长": parseDuration,
	"大小": func(s string) (float64, error) {
		v, err := parseSize(s)
		return float64(v), err
	},
}
var kindExample = map[string]string{
	"速率": "4Mbps / 500KB/s", "时长": "20m / 90s / 2h", "大小": "15GB / 500MB",
}
var fieldLabel = map[string]string{
	"rate": "rate", "min_rate": "min-rate", "burst": "burst", "rest": "rest",
	"cooldown": "cooldown", "daily_cap": "daily-cap",
	"min_size": "min-size", "max_size": "max-size",
}

// kindOrder 决定「量纲填错」时的提示优先级，必须是有序切片。
//
// 早先这里直接遍历 kindParser 这个 map，而 `20m` 既能被解析成时长（20 分钟）
// 也能被解析成大小（20 MB）——于是 `rate: 20m` 报出来的提示在两次运行之间都不一样，
// 有时说「看起来是时长」有时说「看起来是大小」。测试第一次跑就撞上了。
var kindOrder = []string{"时长", "大小", "速率"}

// parseConfigured 按字段量纲解析；失败时尝试猜测是不是填错了字段。
func parseConfigured(field, text string) (float64, error) {
	if text == "" {
		return 0, nil
	}
	kind := fieldKind[field]
	label := fieldLabel[field]
	if v, err := kindParser[kind](text); err == nil {
		return v, nil
	}
	for _, otherKind := range kindOrder {
		if otherKind == kind {
			continue
		}
		if _, err := kindParser[otherKind](text); err == nil {
			return 0, fmt.Errorf(
				"%s 的值 %q 看起来是%s，但 %s 这里需要的是%s（例: %s）。\n"+
					"提示：burst/rest/cooldown 控制的是「传多久、歇多久」的时长；"+
					"真正的限速只由 rate 一个字段控制。",
				label, text, otherKind, label, kind, kindExample[kind])
		}
	}
	return 0, fmt.Errorf("%s 的值 %q 无法解析为%s（例: %s）",
		label, text, kind, kindExample[kind])
}

// dispWidth 计算字符串在终端里的显示列宽（CJK 全角算两列）。
func dispWidth(s string) int { return runewidth.StringWidth(s) }

// fitDisplay 截断到恰好 width 列（超长用 … 收尾），不足补空格——
// 顺带擦掉上一行进度条的残留。
func fitDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if w := dispWidth(s); w <= width {
		return s + strings.Repeat(" ", width-w)
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		cw := runewidth.RuneWidth(r)
		if used+cw > width-1 {
			break
		}
		b.WriteRune(r)
		used += cw
	}
	res := b.String() + "…"
	if pad := width - dispWidth(res); pad > 0 {
		res += strings.Repeat(" ", pad)
	}
	return res
}

// utf16Len 返回字符串的 UTF-16 编码单元数，Telegram 消息实体的 offset/length 用它。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// expandTilde 展开 ~ 与 ~/xxx；无法取得家目录时原样返回。
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
