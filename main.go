package main

// 顶层编排：登录 → premium 检测 → 超限文件切割决策 → 逐文件上传/发送 → 台账记账。
// 对应 tgup.py 的 run() / send_one / send_in_parts / main()。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"mime"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// uploader 汇总发送阶段需要共享的状态。
type uploader struct {
	o         *Options
	client    *telegram.Client
	api       *tg.Client
	ref       *invokerRef // 上传分片用的 invoker（连接池或主连接）
	thumbRef  *invokerRef // 缩略图固定走主连接
	pacer     *Pacer
	ledger    *Ledger
	entity    tg.InputPeerClass
	thumbAt   *ThumbAt
	maxParts  int
	sizeLimit int64
}

func main() {
	o, err := resolveArgs(os.Args[1:])
	switch {
	case errors.Is(err, flag.ErrHelp):
		return // 用法已由 flag 打印
	case errors.Is(err, errFlagParse):
		os.Exit(2) // 错误与用法已由 flag 打印
	case err != nil:
		logf("错误: %v", err)
		os.Exit(2)
	}

	if o.ShowVersion {
		printVersion()
		return
	}

	if o.DumpConfig {
		text := dumpConfig(o)
		if o.Out != "" {
			dest := expandTilde(o.Out)
			_ = os.MkdirAll(filepath.Dir(dest), 0o755)
			if err := os.WriteFile(dest, []byte(text), 0o600); err != nil {
				fatal("写入失败: %v", err)
			}
			logf("已写入 %s（UTF-8, LF）", dest)
		} else {
			fmt.Fprint(os.Stdout, text)
		}
		return
	}
	os.Exit(run(o))
}

func (o *Options) credentials() (int, string) {
	apiID := o.APIID
	apiHash := o.APIHash
	if apiID == "" {
		apiID = os.Getenv("TG_API_ID")
	}
	if apiHash == "" {
		apiHash = os.Getenv("TG_API_HASH")
	}
	if apiID == "" || apiHash == "" {
		fatal("缺少凭据：设置 TG_API_ID / TG_API_HASH，或用 --api-id / --api-hash")
	}
	id, err := strconv.Atoi(strings.TrimSpace(apiID))
	if err != nil {
		fatal("api_id 不是数字: %q", apiID)
	}
	return id, apiHash
}

func run(o *Options) int {
	started := time.Now()
	ctx, stopSignals := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// ---- 组装节流参数（解析失败给清晰提示而不是裸堆栈）
	rate, err := parseConfigured("rate", o.Rate)
	if err != nil {
		fatal("%v", err)
	}
	burstS, err := parseConfigured("burst", o.Burst)
	if err != nil {
		fatal("%v", err)
	}
	restS, err := parseConfigured("rest", o.Rest)
	if err != nil {
		fatal("%v", err)
	}
	if (burstS > 0) != (restS > 0) {
		fatal("--burst 与 --rest 必须成对出现")
	}
	windows := make([]TimeWindow, 0, len(o.Window))
	for _, w := range o.Window {
		tw, err := parseWindow(w)
		if err != nil {
			fatal("%v", err)
		}
		windows = append(windows, tw)
	}
	dailyCapF, err := parseConfigured("daily_cap", o.DailyCap)
	if err != nil {
		fatal("%v", err)
	}
	dailyCap := int64(dailyCapF)
	minSizeF, err := parseConfigured("min_size", o.MinSize)
	if err != nil {
		fatal("%v", err)
	}
	maxSizeF, err := parseConfigured("max_size", o.MaxSize)
	if err != nil {
		fatal("%v", err)
	}
	thumbAt, err := parseThumbAt(o.ThumbAt)
	if err != nil {
		fatal("%v", err)
	}
	cooldownS, err := parseConfigured("cooldown", o.Cooldown)
	if err != nil {
		fatal("%v", err)
	}
	minRate, err := parseConfigured("min_rate", o.MinRate)
	if err != nil {
		fatal("%v", err)
	}
	// --part-size 的合法性已在 Options.validate() 里查过，这里不再重复。

	exts := map[string]struct{}{}
	extSrc := o.Ext
	if extSrc == "" {
		extSrc = VIDEO_EXTS
	}
	for _, e := range strings.Split(extSrc, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			exts[strings.TrimPrefix(e, ".")] = struct{}{}
		}
	}

	files := discover(o.Inputs, o.Recursive, exts,
		int64(minSizeF), int64(maxSizeF), o.Sort, o.Reverse)

	ledger := openLedger(expandTilde(o.Ledger))
	target := o.To
	if !o.Force {
		before := len(files)
		kept := files[:0]
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil {
				continue
			}
			if !ledger.IsDone(f, fi.Size(), fi.ModTime().Unix(), target) {
				kept = append(kept, f)
			}
		}
		if before-len(kept) > 0 {
			logf("台账命中，跳过已上传 %d 个", before-len(kept))
		}
		files = kept
	}

	if len(files) == 0 {
		log("没有需要上传的文件")
		return 0
	}

	if o.Plan {
		printPlan(files, rate, dailyCap, burstS, restS, windows)
		return 0
	}

	if target == "" {
		fatal("缺少 --to")
	}

	// 凭据检查放到这里而不是函数开头：--plan 完全是本地计算，
	// 没有 api_id/api_hash 也应该能看计划（早先会直接报「缺少凭据」）。
	appID, appHash := o.credentials()

	pacer := newPacer(rate, burstS, restS, windows, dailyCap, ledger, o.OnCap,
		!o.NoAdaptive, o.ThrottleFloor, o.ProbeWindow, cooldownS, minRate)

	sessionPath := expandTilde(o.Session)
	_ = os.MkdirAll(filepath.Dir(sessionPath), 0o700)
	if o.SessionString != "" {
		if err := importTelethonSession(ctx, sessionPath, o.SessionString); err != nil {
			fatal("%v", err)
		}
	}

	resolver, err := proxyResolver(o.Proxy)
	if err != nil {
		fatal("%v", err)
	}
	clientOpts := telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
		RetryInterval:  5 * time.Second,
	}
	if resolver != nil {
		clientOpts.Resolver = resolver
	}
	if o.Verbose {
		clientOpts.Logger = verboseLogger()
	}
	client := telegram.NewClient(appID, appHash, clientOpts)

	u := &uploader{
		o: o, client: client, pacer: pacer, ledger: ledger, thumbAt: thumbAt,
	}

	runErr := client.Run(ctx, func(ctx context.Context) error {
		return u.runInside(ctx, files, target, rate, burstS, restS, dailyCap, started)
	})

	ledger.Flush()
	switch {
	case runErr == nil:
		return 0
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		log("\n已中断（分片进度与台账均已落盘，重跑即续）")
		return 130
	default:
		logf("错误: %v", runErr)
		return 1
	}
}

// ---- client.Run 之内 -------------------------------------------------------

func (u *uploader) runInside(ctx context.Context, files []string, target string,
	rate, burstS, restS float64, dailyCap int64, started time.Time) error {
	o := u.o
	status, err := u.client.Auth().Status(ctx)
	if err != nil {
		return err
	}
	if !status.Authorized {
		flow := auth.NewFlow(termAuth{}, auth.SendCodeOptions{})
		if err := u.client.Auth().IfNecessary(ctx, flow); err != nil {
			return err
		}
		if status, err = u.client.Auth().Status(ctx); err != nil {
			return err
		}
	}
	me := status.User
	who := fmt.Sprint(me.ID)
	if me.Username != "" {
		who = me.Username
	}
	logf("已登录: %s (@%s)", me.FirstName, who)

	api := tg.NewClient(u.client)
	u.api = api
	// tg.Invoker 的实现是 telegram.Client 本体（tg.Client 只是 raw API 门面）
	u.ref = &invokerRef{inv: u.client}
	u.thumbRef = &invokerRef{inv: u.client}

	if o.ListChats > 0 {
		return listChats(ctx, api, o.ListChats)
	}

	premium := me.Premium
	sizeLimit := int64(2) * GB
	u.maxParts = MAX_PARTS
	if premium {
		sizeLimit = 4 * GB
		u.maxParts = MAX_PARTS_PREMIUM
	}
	u.sizeLimit = sizeLimit

	// 超限文件：切割或跳过
	var oversized []string
	for _, f := range files {
		if fileSize(f) > sizeLimit {
			oversized = append(oversized, f)
		}
	}
	if len(oversized) > 0 {
		hasFF := hasTool("ffmpeg") && hasTool("ffprobe")
		if o.NoSplit || !hasFF {
			why := "未找到 ffmpeg/ffprobe"
			if o.NoSplit {
				why = "--no-split"
			}
			logf("⚠ %s，以下超过 %s 的文件将被跳过：", why, human(float64(sizeLimit)))
			for _, f := range oversized {
				logf("    %9s  %s", human(float64(fileSize(f))), filepath.Base(f))
			}
			oversizedSet := map[string]bool{}
			for _, f := range oversized {
				oversizedSet[f] = true
			}
			kept := files[:0]
			for _, f := range files {
				if !oversizedSet[f] {
					kept = append(kept, f)
				}
			}
			files = kept
		} else {
			logf("发现 %d 个超过 %s 的文件，将在上传前无损切段，全部传完后自动删除分片：",
				len(oversized), human(float64(sizeLimit)))
			for i, f := range oversized {
				if i == 10 {
					logf("    … 另有 %d 个", len(oversized)-10)
					break
				}
				logf("    %9s  %s", human(float64(fileSize(f))), filepath.Base(f))
			}
		}
	}

	// 多连接池（惰性建连，最多 connections-1 条附加连接 + 1 条主连接）
	var pool telegram.CloseInvoker
	if o.Connections > 1 {
		if p, err := u.client.Pool(int64(o.Connections - 1)); err == nil {
			pool = p
			u.ref.set(p)
			logf("已启用 %d 条上传连接（含主连接）", o.Connections)
		} else {
			logf("  ⚠ 多连接不可用（%v），回退单连接", err)
		}
	}
	// 长静默（占空比 rest / 时间窗 / 冷却）期间把附加连接收掉，醒来再建
	pacer := u.pacer
	pacer.OnPause = func(ctx context.Context) error {
		if pool != nil {
			_ = pool.Close()
			pool = nil
			u.ref.set(u.client)
		}
		return nil
	}
	pacer.OnWake = func(ctx context.Context) error {
		if o.Connections > 1 && pool == nil {
			if p, err := u.client.Pool(int64(o.Connections - 1)); err == nil {
				pool = p
				u.ref.set(p)
				logf("  ↻ 已重建 %d 条上传连接", o.Connections)
			}
		}
		return nil
	}

	if o.Concurrency < o.Connections {
		logf("  提示：concurrency(%d) < connections(%d)，连接跑不满，已自动抬到 %d",
			o.Concurrency, o.Connections, o.Connections)
		o.Concurrency = o.Connections
	}

	entity, err := resolveTarget(ctx, api, target)
	if err != nil {
		return err
	}
	u.entity = entity

	var total int64
	for _, f := range files {
		total += fileSize(f)
	}
	msg := fmt.Sprintf("\n开始上传 %d 个文件 / %s", len(files), human(float64(total)))
	if rate > 0 {
		msg += fmt.Sprintf("，限速 %s/s", human(rate))
	}
	if burstS > 0 {
		msg += fmt.Sprintf("，占空比 %s:%s", humanDur(burstS), humanDur(restS))
	}
	log(msg)
	if dailyCap > 0 {
		logf("今日已用配额 %s / %s", human(float64(u.ledger.UsedToday())), human(float64(dailyCap)))
	}
	if !o.Video && !o.ForceDocument && len(files) > 0 {
		allVideo := true
		for _, f := range files {
			if _, ok := videoMime[strings.TrimPrefix(
				strings.ToLower(filepath.Ext(f)), ".")]; !ok {
				allVideo = false
				break
			}
		}
		if allVideo {
			log("提示：这些看起来都是视频，但没加 --video，会以普通文件形式上传" +
				"（没有播放按钮）。想要内联播放请加 --video。")
		}
	}

	ok := 0
loop:
	for i, src := range files {
		prefix := fmt.Sprintf("[%d/%d] ", i+1, len(files))
		size := fileSize(src)
		logf("\n%s%s  (%s)", prefix, src, human(float64(size)))
		fi, err := os.Stat(src)
		if err != nil {
			logf("  ⚠ 无法读取文件信息，跳过: %v", err)
			continue
		}
		mtime := fi.ModTime().Unix()
		caption := o.Caption
		if caption == "" && o.NameCaption {
			caption = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		}

		var done bool
		if size > sizeLimit {
			done, err = u.sendInParts(ctx, src, size, mtime, prefix, caption)
		} else {
			var mid int64
			mid, err = u.sendOne(ctx, src, size, mtime, prefix, caption)
			if err == nil && mid > 0 {
				u.ledger.Mark(src, size, mtime, target, mid)
			}
			done = mid > 0
		}
		if err != nil {
			var dc *dailyCapError
			if errors.As(err, &dc) {
				logf("\n■ %v", err)
				if o.OnCap == "stop" {
					log("  已停止；明天重跑同一命令会自动接着传")
					break loop
				}
				now := time.Now()
				nxt := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 1, 0, 0, now.Location())
				logf("  休眠至 %s", nxt.Format("01-02 15:04"))
				if err := sleepCtx(ctx, nxt.Sub(now)); err != nil {
					return err
				}
				continue // 与 Python 一致：休眠后从下一个文件继续
			}
			var se *splitError
			if errors.As(err, &se) {
				logf("  ✗ 切割失败，跳过该文件：%v", err)
				continue
			}
			var oe *oversizeError
			if errors.As(err, &oe) {
				logf("  ✗ %v，跳过该文件", err)
				continue
			}
			return err
		}

		if done {
			ok++
		}
		if i < len(files)-1 {
			if err := u.pacer.InterFilePause(ctx, o.GapMin, o.GapMax); err != nil {
				return err
			}
		}
	}

	el := time.Since(started).Seconds()
	logf("\n完成 %d/%d  耗时 %s（其中主动静默 %s）",
		ok, len(files), humanDur(el), humanDur(u.pacer.PausedTotal()))
	if dailyCap > 0 {
		logf("今日累计 %s / %s", human(float64(u.ledger.UsedToday())), human(float64(dailyCap)))
	}
	return nil
}

// ---- 发送一个物理文件 -------------------------------------------------------

func (u *uploader) upload(ctx context.Context, src string, size, mtime int64,
	resume bool, prefix string) (tg.InputFileClass, error) {
	partSize := 0
	if u.o.PartSize > 0 {
		partSize = int(u.o.PartSize * KB)
	}
	return uploadFile(ctx, src, size, mtime, uploadOpts{
		partSize:    partSize,
		concurrency: u.o.Concurrency,
		retries:     u.o.Retries,
		stateDir:    expandTilde(u.o.StateDir),
		maxParts:    u.maxParts,
		resume:      resume,
		pacer:       u.pacer,
		pool:        u.ref,
		prefix:      prefix,
	})
}

// sendOne 传一个物理文件并发出去，返回 message_id；发送失败返回 0。
func (u *uploader) sendOne(ctx context.Context, one string, size, mtime int64,
	prefix, caption string) (int64, error) {
	handle, err := u.upload(ctx, one, size, mtime, !u.o.NoResume, prefix)
	if err != nil {
		return 0, err
	}

	var vmeta *videoMeta
	if u.o.Video {
		vmeta = probeVideo(ctx, one)
	}
	var thumb tg.InputFileClass
	if vmeta != nil && !u.o.Photo && !u.o.NoThumb {
		thumb = u.uploadThumbnail(ctx, one, vmeta.Duration)
	}

	reuploaded := false
	sent := false
	var msgID int64
	for attempt := 0; attempt < u.o.Retries; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		media := u.buildMedia(handle, one, vmeta, thumb)
		text, ents := buildCaption(caption, u.o.ParseMode)
		updates, err := u.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer:     u.entity,
			Media:    media,
			Message:  text,
			Entities: ents,
			RandomID: randInt64(),
			Silent:   u.o.Silent,
		})
		if err == nil {
			if id, ok := extractMessageID(updates); ok {
				msgID = id
			}
			sent = true
			break
		}
		if d, ok := isFloodWait(err); ok {
			logf("  ⏳ 发送 FloodWait %ds", int(d.Seconds())+1)
			if err := sleepCtx(ctx, d+time.Second); err != nil {
				return 0, err
			}
			continue
		}
		if isFilePartMissing(err) {
			// 所有分片都"传完"了，服务端却说某片不在——本地续传状态记的是
			// 上一轮（甚至上一天）传的片，服务端早把它们回收了。状态作废，
			// 整份重传一次；再不行就是别的毛病，跳过这个文件而不是崩掉整批。
			if reuploaded {
				logf("  ✗ 重传后仍报 FILE_PART_MISSING: %v，跳过", err)
				return 0, nil
			}
			reuploaded = true
			logf("\n  ⚠ 服务端已丢弃先前上传的分片（%v）\n    → 作废续传状态，整份重新上传", err)
			discardState(expandTilde(u.o.StateDir), one, size, mtime)
			if handle, err = u.upload(ctx, one, size, mtime, false, prefix); err != nil {
				return 0, err
			}
			continue
		}
		bo := float64(min(1<<attempt, 30)) + rand.Float64()
		logf("  ⚠ 发送失败 %v，%.1fs 后重试", err, bo)
		if err := sleepCtx(ctx, time.Duration(bo*float64(time.Second))); err != nil {
			return 0, err
		}
	}
	if !sent {
		log("  ✗ 发送失败，跳过")
		return 0, nil
	}
	logf("  ✓ message_id=%d", msgID)
	return msgID, nil
}

// sendInParts 切段后逐段上传。全部成功才清理分片并记账，否则原样留着等重跑。
func (u *uploader) sendInParts(ctx context.Context, src string, size, mtime int64,
	prefix, caption string) (bool, error) {
	splitDir := ""
	if u.o.SplitDir != "" {
		splitDir = expandTilde(u.o.SplitDir)
	}
	parts, err := splitFile(ctx, src, u.sizeLimit, splitDir)
	if err != nil {
		return false, err
	}
	root := splitRootFor(src, splitDir)
	n := len(parts)
	var lastID int64
	for j, part := range parts {
		pfx := fmt.Sprintf("%s(%d/%d) ", prefix, j+1, n)
		pfi, err := os.Stat(part)
		if err != nil {
			return false, err
		}
		psz, pmtime := pfi.Size(), pfi.ModTime().Unix()
		if !u.o.Force && u.ledger.IsDone(part, psz, pmtime, u.o.To) {
			logf("  ↷ 第 %d/%d 段已在台账中，跳过", j+1, n)
			continue
		}
		logf("\n%s%s  (%s)", pfx, part, human(float64(psz)))
		partCaption := caption
		if partCaption != "" {
			partCaption = fmt.Sprintf("%s [%d/%d]", partCaption, j+1, n)
		}
		mid, err := u.sendOne(ctx, part, psz, pmtime, pfx, partCaption)
		if err != nil {
			// 配额用尽 / Ctrl-C / 网络彻底断了：分片不能删，删了下次要从头切
			logf("  ℹ 分片留在 %s，重跑会复用并跳过已传的段", root)
			return false, err
		}
		if mid == 0 {
			logf("  ℹ 分片留在 %s，重跑会复用并跳过已传的段", root)
			return false, nil
		}
		u.ledger.Mark(part, psz, pmtime, u.o.To, mid)
		lastID = mid
		if j < n-1 {
			if err := u.pacer.InterFilePause(ctx, u.o.GapMin, u.o.GapMax); err != nil {
				logf("  ℹ 分片留在 %s，重跑会复用并跳过已传的段", root)
				return false, err
			}
		}
	}

	// 整片记账要在清理之前：先把「这个源文件已经传完」落盘，
	// 万一删目录时出岔子，也不会导致下次重传一遍。
	u.ledger.Mark(src, size, mtime, u.o.To, lastID)
	if u.o.KeepSplits {
		logf("  ✓ %d 段全部上传完成，分片保留在 %s", n, root)
	} else {
		cleanupSplit(root)
		logf("  ✓ %d 段全部上传完成，已清理分片目录", n)
	}
	return true, nil
}

// buildMedia 组装 InputMedia。
func (u *uploader) buildMedia(file tg.InputFileClass, src string,
	vmeta *videoMeta, thumb tg.InputFileClass) tg.InputMediaClass {
	name := filepath.Base(src)
	if u.o.Photo {
		photo := &tg.InputMediaUploadedPhoto{File: file}
		if u.o.TTL > 0 {
			photo.TTLSeconds = int(u.o.TTL)
		}
		return photo
	}
	var mimeType string
	if vmeta != nil {
		mimeType = guessVideoMime(name)
	} else {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}
	attrs := []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}}
	if vmeta != nil && !u.o.ForceDocument {
		attrs = append(attrs, &tg.DocumentAttributeVideo{
			Duration:          float64(vmeta.Duration),
			W:                 vmeta.W,
			H:                 vmeta.H,
			SupportsStreaming: true,
		})
	}
	doc := &tg.InputMediaUploadedDocument{
		File:       file,
		MimeType:   mimeType,
		Attributes: attrs,
		ForceFile:  u.o.ForceDocument,
	}
	if thumb != nil {
		doc.Thumb = thumb
	}
	if u.o.TTL > 0 {
		doc.TTLSeconds = int(u.o.TTL)
	}
	return doc
}

// uploadThumbnail 生成并上传缩略图。走主连接、不经 Pacer：
// 缩略图只有几十 KB，计入整形反而是噪音。
func (u *uploader) uploadThumbnail(ctx context.Context, src string, duration int) tg.InputFileClass {
	path := makeThumbnail(ctx, src, duration, u.thumbAt)
	if path == "" {
		return nil
	}
	defer os.Remove(path)
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	file, err := uploadFile(ctx, path, fi.Size(), fi.ModTime().Unix(), uploadOpts{
		concurrency: 1,
		retries:     3,
		maxParts:    u.maxParts,
		resume:      false,
		pool:        u.thumbRef,
		name:        "thumb.jpg",
	})
	if err != nil {
		logf("  ⚠ %s: 缩略图上传失败（%v），继续传正片", filepath.Base(src), err)
		return nil
	}
	return file
}
