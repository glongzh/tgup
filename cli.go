package main

// 命令行参数与三层合并（内置默认 < 配置文件+profile < 命令行显式参数）。
// 对应 tgup.py 的 build_parser / resolve_args / explicit_cli_args。

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Options 是合并后的全部参数。字符串形态的量纲字段在 run() 里才解析，
// 这样配置文件与命令行可以走同一条解析/报错路径。
type Options struct {
	Inputs []string
	To     string

	Recursive bool
	Ext       string
	MinSize   string
	MaxSize   string
	Sort      string
	Reverse   bool

	Rate     string
	Burst    string
	Rest     string
	Window   []string
	DailyCap string
	OnCap    string
	GapMin   float64
	GapMax   float64

	NoAdaptive    bool
	ThrottleFloor float64
	ProbeWindow   float64
	Cooldown      string
	MinRate       string

	NoSplit    bool
	SplitDir   string
	KeepSplits bool

	Caption       string
	NameCaption   bool
	ParseMode     string
	ForceDocument bool
	Photo         bool
	Video         bool
	NoThumb       bool
	ThumbAt       string
	TTL           int64
	Silent        bool

	PartSize    int64
	Connections int
	Concurrency int
	Retries     int
	NoResume    bool
	StateDir    string
	Ledger      string
	Force       bool

	Session       string
	SessionString string
	APIID         string
	APIHash       string
	Proxy         string
	Verbose       bool

	FromConfig  string
	Profile     string
	DumpConfig  bool
	Out         string
	Plan        bool
	ListChats   int // 0 = 不列出；>0 = 列出条数（裸 --list-chats 即 50）
	ShowVersion bool
}

type windowList []string

func (w *windowList) String() string     { return strings.Join(*w, ",") }
func (w *windowList) Set(s string) error { *w = append(*w, s); return nil }

type listChatsVal struct{ dst *int }

func (l *listChatsVal) String() string {
	if l.dst == nil || *l.dst <= 0 {
		return ""
	}
	return strconv.Itoa(*l.dst)
}
func (l *listChatsVal) Set(s string) error {
	if s == "true" {
		*l.dst = 50 // 裸 --list-chats
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("需要数字: %q", s)
	}
	*l.dst = n
	return nil
}
func (l *listChatsVal) IsBoolFlag() bool { return true }

// shortAlias 把短选项归一化到长选项名。
var shortAlias = map[string]string{
	"r": "recursive", "c": "caption", "d": "force-document", "o": "out",
}

func canonicalDest(flagName string) string {
	if c, ok := shortAlias[flagName]; ok {
		return c
	}
	return flagName
}

func newFlagSet(o *Options) *flag.FlagSet {
	// ContinueOnError 而不是 ExitOnError：解析结果由调用方决定怎么处理，
	// 也让 resolveArgs 能在测试里直接调用。
	fs := flag.NewFlagSet("tgup", flag.ContinueOnError)

	// 位置参数 inputs 在 Parse 后从 fs.Args() 取
	fs.StringVar(&o.To, "to", "", "目标：me / @username / 数字 id")
	fs.BoolVar(&o.Recursive, "recursive", false, "递归子目录")
	fs.StringVar(&o.Ext, "ext", "", "扩展名白名单，逗号分隔（默认 "+VIDEO_EXTS+"）")
	fs.StringVar(&o.MinSize, "min-size", "", "忽略小于该大小的文件，如 10MB")
	fs.StringVar(&o.MaxSize, "max-size", "", "忽略大于该大小的文件")
	fs.StringVar(&o.Sort, "sort", "mtime", "排序: name / size / mtime")
	fs.BoolVar(&o.Reverse, "reverse", false, "倒序")

	fs.StringVar(&o.Rate, "rate", "", "限速，如 4Mbps / 500KB/s。小写 b=比特 大写 B=字节")
	fs.StringVar(&o.Burst, "burst", "", "连续上传时长，如 20m")
	fs.StringVar(&o.Rest, "rest", "", "静默时长，如 10m。须与 --burst 同时给")
	fs.Var((*windowList)(&o.Window), "window", "允许时段 HH:MM-HH:MM，可重复")
	fs.StringVar(&o.DailyCap, "daily-cap", "", "每日上传上限，如 15GB")
	fs.StringVar(&o.OnCap, "on-cap", "stop", "配额用尽后：stop 退出 / wait 等到次日")
	fs.Float64Var(&o.GapMin, "gap-min", 20, "文件间隔下限（秒）")
	fs.Float64Var(&o.GapMax, "gap-max", 90, "文件间隔上限（秒）")

	fs.BoolVar(&o.NoAdaptive, "no-adaptive", false, "关闭吞吐塌陷检测")
	fs.Float64Var(&o.ThrottleFloor, "throttle-floor", 0.4, "实测/目标 低于该比值即判定被限速")
	fs.Float64Var(&o.ProbeWindow, "probe-window", 180, "采样窗口（秒）")
	fs.StringVar(&o.Cooldown, "cooldown", "15m", "判定被限速后的冷却时长")
	fs.StringVar(&o.MinRate, "min-rate", "", "自适应降速的下限")

	fs.BoolVar(&o.NoSplit, "no-split", false, "超上限文件直接跳过，不切割（默认无损切段逐段上传）")
	fs.StringVar(&o.SplitDir, "split-dir", "", "分片目录的存放位置（默认放在源文件旁边）")
	fs.BoolVar(&o.KeepSplits, "keep-splits", false, "全部传完后保留分片文件（默认自动删除）")

	fs.StringVar(&o.Caption, "caption", "", "统一说明文字")
	fs.BoolVar(&o.NameCaption, "name-caption", false, "用文件名（去扩展名）作为说明文字")
	fs.StringVar(&o.ParseMode, "parse-mode", "md", "说明文字解析: md / html / none")
	fs.BoolVar(&o.ForceDocument, "force-document", false, "以普通文件形式发送（无播放按钮）")
	fs.BoolVar(&o.Photo, "photo", false, "按照片发送")
	fs.BoolVar(&o.Video, "video", false, "ffprobe 探测时长/分辨率，支持内联播放")
	fs.BoolVar(&o.NoThumb, "no-thumb", false, "不生成缩略图（默认在 --video 时自动抽帧上传）")
	fs.StringVar(&o.ThumbAt, "thumb-at", "",
		"缩略图取帧位置：10% 按时长比例，或 90s / 1.5m 绝对时间点；默认自动挑第一个非黑帧")
	fs.Int64Var(&o.TTL, "ttl", 0, "定时自毁秒数")
	fs.BoolVar(&o.Silent, "silent", false, "静默发送（不触发通知）")

	fs.Int64Var(&o.PartSize, "part-size", 0, "分片大小（KB），默认按文件大小自适应")
	fs.IntVar(&o.Connections, "connections", 4,
		fmt.Sprintf("到 DC 的 TCP 连接数（默认 4，上限 %d）。跨洋高 RTT 链路上这是决定速度的主要因素",
			MAX_CONNECTIONS))
	fs.IntVar(&o.Concurrency, "concurrency", 4, "并发在途分片数（默认 4）。应 >= connections")
	fs.IntVar(&o.Retries, "retries", 5, "分片/发送失败重试次数（必须 >= 1）")
	fs.BoolVar(&o.NoResume, "no-resume", false, "关闭分片级断点续传")
	fs.StringVar(&o.StateDir, "state-dir", expandTilde(DEFAULT_STATE_DIR), "续传状态目录")
	fs.StringVar(&o.Ledger, "ledger", expandTilde(DEFAULT_LEDGER), "上传台账路径")
	fs.BoolVar(&o.Force, "force", false, "忽略台账，重复上传")

	fs.StringVar(&o.Session, "session", expandTilde(DEFAULT_SESSION), "gotd 会话文件路径（JSON）")
	fs.StringVar(&o.SessionString, "session-string", "",
		"一次性导入 Telethon StringSession（优先于已有会话文件）")
	fs.StringVar(&o.APIID, "api-id", "", "Telegram api_id（缺省读 TG_API_ID）")
	fs.StringVar(&o.APIHash, "api-hash", "", "Telegram api_hash（缺省读 TG_API_HASH）")
	fs.StringVar(&o.Proxy, "proxy", "", "代理，如 socks5://user:pass@host:1080 或 http://host:8080")
	fs.BoolVar(&o.Verbose, "verbose", false, "打印 gotd 的全部网络日志")

	fs.StringVar(&o.FromConfig, "from-config", "", "从 yaml/toml/json 读取参数；命令行显式参数优先级更高")
	fs.StringVar(&o.Profile, "profile", "", "叠加配置文件 profiles 段里的指定档位")
	fs.BoolVar(&o.DumpConfig, "dump-config", false, "按当前参数输出一份带注释的 YAML 模板后退出")
	fs.StringVar(&o.Out, "out", "", "--dump-config 的输出文件（UTF-8, LF）；不给则打到 stdout")
	fs.BoolVar(&o.Plan, "plan", false, "只打印计划与耗时预估")
	fs.Var(&listChatsVal{dst: &o.ListChats},
		"list-chats", "列出会话后退出（裸用 = 50 条；--list-chats=100 指定条数）")
	fs.BoolVar(&o.ShowVersion, "version", false, "打印版本信息后退出")

	// 短别名（flag 包不支持组合注册，分开注册指向同一变量即可）
	fs.BoolVar(&o.Recursive, "r", false, "同 --recursive")
	fs.StringVar(&o.Caption, "c", "", "同 --caption")
	fs.BoolVar(&o.ForceDocument, "d", false, "同 --force-document")
	fs.StringVar(&o.Out, "o", "", "同 --out")

	fs.Usage = func() { printUsage(fs) }
	return fs
}

var usageGroups = []struct{ title, items string }{
	{"目标与输入", "to, recursive, ext, min-size, max-size, sort, reverse"},
	{"上行整形（规避 PCDN 判定的核心）", "rate, burst, rest, window, daily-cap, on-cap, gap-min, gap-max"},
	{"限速自检", "no-adaptive, throttle-floor, probe-window, cooldown, min-rate"},
	{"超限视频切割", "no-split, split-dir, keep-splits"},
	{"发送选项", "caption, name-caption, parse-mode, force-document, photo, video, no-thumb, thumb-at, ttl, silent"},
	{"传输参数", "part-size, connections, concurrency, retries, no-resume, state-dir, ledger, force"},
	{"连接", "session, session-string, api-id, api-hash, proxy, verbose"},
	{"动作（执行后退出）", "plan, list-chats, dump-config, out, version"},
	{"配置文件", "from-config, profile"},
}

func printUsage(fs *flag.FlagSet) {
	// 短别名归并到它对应的长选项上，避免 -r 与 --recursive 各占一行。
	// usageGroups 里因此只列长选项名。
	aliasOf := map[string][]string{}
	for short, long := range shortAlias {
		aliasOf[long] = append(aliasOf[long], short)
	}
	for _, s := range aliasOf {
		sort.Strings(s)
	}

	out := os.Stderr
	fmt.Fprintln(out, "tgup — 批量上传视频到 Telegram（gotd/MTProto），带上行整形以规避 PCDN 误判")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "用法: tgup [选项] 文件或目录...")
	fmt.Fprintln(out)
	for _, g := range usageGroups {
		fmt.Fprintf(out, "%s:\n", g.title)
		for _, name := range strings.Split(g.items, ", ") {
			if len(name) == 1 {
				continue // 短别名随长选项一起显示
			}
			f := fs.Lookup(name)
			if f == nil {
				continue
			}
			prefix := "    --" + f.Name
			for _, s := range aliasOf[f.Name] {
				prefix += ", -" + s
			}
			if f.DefValue != "" && f.DefValue != "false" {
				prefix += fmt.Sprintf(" (默认 %s)", f.DefValue)
			}
			fmt.Fprintf(out, "%-44s %s\n", prefix, f.Usage)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "环境变量:")
	fmt.Fprintln(out, "  TG_API_ID, TG_API_HASH     Telegram API 凭据（推荐用环境变量而不是配置文件）")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "示例:")
	fmt.Fprintln(out, "  tgup --to me -r --plan ~/Videos ~/Movies")
	fmt.Fprintln(out, "  tgup --to @mychannel -r --rate 4Mbps --burst 20m --rest 10m \\")
	fmt.Fprintln(out, "       --window 09:00-23:30 --daily-cap 15GB --connections 4 --video ~/Videos")
	fmt.Fprintln(out, "  tgup --dump-config > tgup.yaml && tgup --from-config tgup.yaml")
}

// applyConfigKey 把配置文件里的一个键写进 Options。
func applyConfigKey(o *Options, key string, val any) error {
	var err error
	switch key {
	case "to":
		o.To = toStr(val)
	case "inputs":
		o.Inputs = toList(val)
	case "recursive":
		o.Recursive, err = toBool(val)
	case "ext":
		o.Ext = toStr(val)
	case "min_size":
		o.MinSize = toStr(val)
	case "max_size":
		o.MaxSize = toStr(val)
	case "sort":
		o.Sort = toStr(val)
	case "reverse":
		o.Reverse, err = toBool(val)
	case "rate":
		o.Rate = toStr(val)
	case "burst":
		o.Burst = toStr(val)
	case "rest":
		o.Rest = toStr(val)
	case "window":
		o.Window = toList(val)
	case "daily_cap":
		o.DailyCap = toStr(val)
	case "on_cap":
		o.OnCap = toStr(val)
	case "gap_min":
		o.GapMin, err = toFloat(val)
	case "gap_max":
		o.GapMax, err = toFloat(val)
	case "no_adaptive":
		o.NoAdaptive, err = toBool(val)
	case "throttle_floor":
		o.ThrottleFloor, err = toFloat(val)
	case "probe_window":
		o.ProbeWindow, err = toFloat(val)
	case "cooldown":
		o.Cooldown = toStr(val)
	case "min_rate":
		o.MinRate = toStr(val)
	case "no_split":
		o.NoSplit, err = toBool(val)
	case "split_dir":
		o.SplitDir = toStr(val)
	case "keep_splits":
		o.KeepSplits, err = toBool(val)
	case "caption":
		o.Caption = toStr(val)
	case "name_caption":
		o.NameCaption, err = toBool(val)
	case "parse_mode":
		o.ParseMode = toStr(val)
	case "force_document":
		o.ForceDocument, err = toBool(val)
	case "photo":
		o.Photo, err = toBool(val)
	case "video":
		o.Video, err = toBool(val)
	case "no_thumb":
		o.NoThumb, err = toBool(val)
	case "thumb_at":
		o.ThumbAt = toStr(val)
	case "ttl":
		o.TTL, err = toInt(val)
	case "silent":
		o.Silent, err = toBool(val)
	case "part_size":
		o.PartSize, err = toInt(val)
	case "connections":
		var n int64
		if n, err = toInt(val); err == nil {
			o.Connections = int(n)
		}
	case "concurrency":
		var n int64
		if n, err = toInt(val); err == nil {
			o.Concurrency = int(n)
		}
	case "retries":
		var n int64
		if n, err = toInt(val); err == nil {
			o.Retries = int(n)
		}
	case "no_resume":
		o.NoResume, err = toBool(val)
	case "state_dir":
		o.StateDir = toStr(val)
	case "ledger":
		o.Ledger = toStr(val)
	case "force":
		o.Force, err = toBool(val)
	case "session":
		o.Session = toStr(val)
	case "api_id":
		o.APIID = toStr(val)
	case "api_hash":
		o.APIHash = toStr(val)
	case "proxy":
		o.Proxy = toStr(val)
	case "verbose":
		o.Verbose, err = toBool(val)
	default:
		return fmt.Errorf("配置项无法识别: %s", key)
	}
	if err != nil {
		return fmt.Errorf("配置项 %s 无效: %w", key, err)
	}
	return nil
}

// fieldFor 供 --dump-config 按键取当前值。
func (o *Options) fieldFor(key string) any {
	switch key {
	case "to":
		return o.To
	case "inputs":
		return o.Inputs
	case "recursive":
		return o.Recursive
	case "ext":
		return o.Ext
	case "min_size":
		return o.MinSize
	case "max_size":
		return o.MaxSize
	case "sort":
		return o.Sort
	case "reverse":
		return o.Reverse
	case "rate":
		return o.Rate
	case "burst":
		return o.Burst
	case "rest":
		return o.Rest
	case "window":
		return o.Window
	case "daily_cap":
		return o.DailyCap
	case "on_cap":
		return o.OnCap
	case "gap_min":
		return o.GapMin
	case "gap_max":
		return o.GapMax
	case "no_adaptive":
		return o.NoAdaptive
	case "throttle_floor":
		return o.ThrottleFloor
	case "probe_window":
		return o.ProbeWindow
	case "cooldown":
		return o.Cooldown
	case "min_rate":
		return o.MinRate
	case "no_split":
		return o.NoSplit
	case "split_dir":
		return o.SplitDir
	case "keep_splits":
		return o.KeepSplits
	case "caption":
		return o.Caption
	case "name_caption":
		return o.NameCaption
	case "parse_mode":
		return o.ParseMode
	case "force_document":
		return o.ForceDocument
	case "photo":
		return o.Photo
	case "video":
		return o.Video
	case "no_thumb":
		return o.NoThumb
	case "thumb_at":
		return o.ThumbAt
	case "ttl":
		return o.TTL
	case "silent":
		return o.Silent
	case "part_size":
		return o.PartSize
	case "connections":
		return o.Connections
	case "concurrency":
		return o.Concurrency
	case "retries":
		return o.Retries
	case "no_resume":
		return o.NoResume
	case "state_dir":
		return o.StateDir
	case "ledger":
		return o.Ledger
	case "force":
		return o.Force
	case "session":
		return o.Session
	case "api_id":
		return o.APIID
	case "api_hash":
		return o.APIHash
	case "proxy":
		return o.Proxy
	case "verbose":
		return o.Verbose
	}
	return nil
}

// validate 做启动期参数校验。
//
// 这些值一旦越界，后果往往不是「报个错」而是「静默地什么都不做」——
// 比如 retries=0 会让每个分片第一次尝试就判失败，整批文件全部跳过，
// 表面上却只打印一句「发送失败，跳过」。宁可在开跑前拦住。
func (o *Options) validate() error {
	switch o.Sort {
	case "name", "size", "mtime":
	default:
		return fmt.Errorf("--sort 只支持 name / size / mtime（当前 %q）", o.Sort)
	}
	switch o.ParseMode {
	case "md", "html", "none":
	default:
		return fmt.Errorf("--parse-mode 只支持 md / html / none（当前 %q）", o.ParseMode)
	}
	switch o.OnCap {
	case "stop", "wait":
	default:
		return fmt.Errorf("--on-cap 只支持 stop / wait（当前 %q）", o.OnCap)
	}
	if o.Retries < 1 {
		return fmt.Errorf("--retries 必须 >= 1（当前 %d）：设为 0 会让每个分片第一次尝试就判失败", o.Retries)
	}
	if o.Concurrency < 1 {
		return fmt.Errorf("--concurrency 必须 >= 1（当前 %d）", o.Concurrency)
	}
	if o.Connections < 1 || o.Connections > MAX_CONNECTIONS {
		return fmt.Errorf("--connections 必须在 1..%d 之间（当前 %d）", MAX_CONNECTIONS, o.Connections)
	}
	if o.GapMin < 0 || o.GapMax < 0 {
		return errors.New("--gap-min / --gap-max 不能为负")
	}
	if o.GapMax < o.GapMin {
		return fmt.Errorf("--gap-max(%g) 不能小于 --gap-min(%g)", o.GapMax, o.GapMin)
	}
	if o.ThrottleFloor <= 0 || o.ThrottleFloor > 1 {
		return fmt.Errorf("--throttle-floor 应落在 (0, 1] 区间（当前 %g）", o.ThrottleFloor)
	}
	if o.ProbeWindow <= 0 {
		return fmt.Errorf("--probe-window 必须 > 0（当前 %g）", o.ProbeWindow)
	}
	if o.TTL < 0 {
		return fmt.Errorf("--ttl 不能为负（当前 %d）", o.TTL)
	}
	if o.ListChats < 0 {
		return fmt.Errorf("--list-chats 不能为负（当前 %d）", o.ListChats)
	}
	if o.PartSize > 0 {
		if o.PartSize > MAX_PART_SIZE/KB {
			return fmt.Errorf("--part-size 过大: %d KB（上限 %d KB）", o.PartSize, MAX_PART_SIZE/KB)
		}
		if err := validatePartSize(int(o.PartSize) * KB); err != nil {
			return err
		}
	}
	return nil
}

// errFlagParse 表示 flag 包已经自己把错误和用法打印到 stderr 了，
// 调用方不要再重复打印一遍。
var errFlagParse = errors.New("命令行参数有误")

// resolveArgs 完成解析与三层合并，返回最终参数。
// 参数用显式传入而不是读 os.Args，方便测试。
func resolveArgs(args []string) (*Options, error) {
	o := &Options{}
	fs := newFlagSet(o)
	if err := fs.Parse(args); err != nil {
		// -h/--help 走到这里时用法已经打印过了，交给调用方静默退出
		if errors.Is(err, flag.ErrHelp) {
			return nil, flag.ErrHelp
		}
		return nil, fmt.Errorf("%w: %w", errFlagParse, err)
	}
	o.Inputs = fs.Args()

	// --version 不需要配置文件、不需要凭据，尽早短路
	if o.ShowVersion {
		return o, nil
	}

	// 记录用户真正在命令行上写了的选项名（短别名已归一化），
	// 用于实现「命令行显式参数 > 配置文件」的覆盖语义。
	cliSet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cliSet[canonicalDest(f.Name)] = true })

	if o.FromConfig != "" {
		cfgPath := expandTilde(o.FromConfig)
		if fi, err := os.Stat(cfgPath); err != nil || fi.IsDir() {
			return nil, fmt.Errorf("%w: %s", errConfigNotFound, cfgPath)
		}
		cfg, err := loadConfig(cfgPath, o.Profile)
		if err != nil {
			return nil, err
		}

		// 命令行显式参数 > 配置文件
		var overridden []string
		for _, k := range sortedKeys(cfg) {
			if cliSet[k] {
				overridden = append(overridden, k)
				continue
			}
			if err := applyConfigKey(o, k, cfg[k]); err != nil {
				return nil, err
			}
		}
		src := filepath.Base(cfgPath)
		if o.Profile != "" {
			src += " [" + o.Profile + "]"
		}
		msg := fmt.Sprintf("配置: %s，生效 %d 项", src, len(cfg))
		if len(overridden) > 0 {
			sort.Strings(overridden)
			msg += fmt.Sprintf("，其中 %s 被命令行覆盖", strings.Join(overridden, ", "))
		}
		log(msg)
	}

	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// printVersion 打印版本信息。version/commit/date 由 -ldflags 注入（见 Makefile）。
func printVersion() {
	fmt.Printf("tgup %s (commit %s, built %s)\n", version, commit, date)
	fmt.Printf("go %s\n", goVersion())
}
