package main

// 超限切割：Telegram 单文件有硬上限（普通号 2GB，Premium 4GB），超了服务端直接拒收。
// 这里在上传前把超限文件切成若干段，逐段发，全部成功后删掉分片。
//
// 一律用 -c copy：不重编码，几个 GB 也就是一次顺序读写的时间，画质无损。
// 代价是只能在关键帧处下刀，段的实际时长会偏离设定值，所以按大小估算切点时
// 留 5% 余量（SPLIT_SAFETY），切完再逐段核对，仍然超限的对半再切。
// 对应 tgup.py 的「超限切割」一节。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// splitError 切割没能完成。调用方跳过这个文件继续下一个，不该让整个批次崩掉。
type splitError struct{ msg string }

func (e *splitError) Error() string { return e.msg }

// ffRun 执行一个外部命令。
//
// ctx 一路从 signal.NotifyContext 传下来，Ctrl-C 才能真正把 ffmpeg 掐掉：
// 切一个几 GB 的片子要跑好几分钟，旧实现内部固定用 context.Background()，
// 那段时间里 Ctrl-C 完全没反应，只能再开一个终端 kill。
func ffRun(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	// 被取消/超时的情况要优先识别：exec.CommandContext 杀进程后返回的是
	// "signal: killed" 这类普通错误，不先看 ctx 就会被归成「找不到 ffmpeg」，
	// 把用户引向完全错误的方向。
	if cerr := runCtx.Err(); cerr != nil {
		if errors.Is(cerr, context.DeadlineExceeded) {
			return "", &splitError{fmt.Sprintf("%s 超时（%ss）", args[0], timeout)}
		}
		return "", cerr
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		var tail []string
		for _, ln := range strings.Split(stderr.String(), "\n") {
			if strings.TrimSpace(ln) != "" {
				tail = append(tail, ln)
			}
		}
		if len(tail) > 3 {
			tail = tail[len(tail)-3:]
		}
		detail := ""
		if len(tail) > 0 {
			detail = "：" + strings.Join(tail, " / ")
		}
		return "", &splitError{fmt.Sprintf("%s 退出码 %d%s", args[0], ee.ExitCode(), detail)}
	}
	return "", &splitError{fmt.Sprintf("找不到 %s，请先安装 ffmpeg", args[0])}
}

func mediaDuration(ctx context.Context, path string) (float64, error) {
	out, err := ffRun(ctx, 120*time.Second, "ffprobe", "-v", "quiet",
		"-print_format", "json", "-show_entries", "format=duration", path)
	if err != nil {
		return 0, err
	}
	var d struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal([]byte(out), &d) != nil || d.Format.Duration == "" {
		return 0, &splitError{fmt.Sprintf("%s: ffprobe 没报出时长，无法按大小估算切点",
			filepath.Base(path))}
	}
	dur, perr := json.Number(d.Format.Duration).Float64()
	if perr != nil {
		return 0, &splitError{fmt.Sprintf("%s: ffprobe 没报出时长，无法按大小估算切点",
			filepath.Base(path))}
	}
	return dur, nil
}

func splitRootFor(src, baseDir string) string {
	// 默认放在源文件旁边（同一块盘，不至于把系统盘写爆），对应 Python 的 src.parent
	parent := filepath.Dir(src)
	if baseDir != "" {
		parent = expandTilde(baseDir)
	}
	return filepath.Join(parent, SPLIT_DIR_PREFIX+strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
}

// segment 只取第一条视频流和全部音频流。不能用 -map 0：有些 mp4 带封面图、
// 时间码等附加流（codec 显示为 none），mp4 容器写不了会直接报
// "Could not find tag for codec none"；字幕 -c copy 进 mp4 也多半要转 mov_text，一并丢掉。
func segment(ctx context.Context, src, pattern string, segS float64) error {
	_, err := ffRun(ctx, 0, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", src, "-c", "copy",
		"-map", "0:v:0", "-map", "0:a?", "-sn", "-dn",
		"-f", "segment", "-segment_time", fmt.Sprintf("%.3f", max(segS, 1.0)),
		"-reset_timestamps", "1", pattern)
	return err
}

type splitManifest struct {
	Src      string   `json:"src"`
	Size     int64    `json:"size"`
	Mtime    int64    `json:"mtime"`
	MaxBytes int64    `json:"max_bytes"`
	Parts    []string `json:"parts"`
	Created  int64    `json:"created"`
}

// loadManifest 复用上次切好的分片。清单是切割全部完成后才写的，所以它存在就意味着
// 这批分片是完整的——中途被 Ctrl-C 掐掉的半成品不会有清单，会被重切。
func loadManifest(root, src string, size, mtime int64) []string {
	data, err := os.ReadFile(filepath.Join(root, SPLIT_MANIFEST))
	if err != nil {
		return nil
	}
	var m splitManifest
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	if m.Size != size || m.Mtime != mtime {
		return nil // 源文件变过了，旧分片作废
	}
	if len(m.Parts) == 0 {
		return nil
	}
	for _, name := range m.Parts {
		if fi, err := os.Stat(filepath.Join(root, name)); err != nil || !fi.Mode().IsRegular() {
			return nil
		}
	}
	return m.Parts
}

// resplitOversized 对仍然超限的段按时长对半再切，直到都达标。
func resplitOversized(ctx context.Context, parts []string, maxBytes int64, depth int) ([]string, error) {
	out := []string{}
	for _, p := range parts {
		size := fileSize(p)
		if size <= maxBytes {
			out = append(out, p)
			continue
		}
		if depth >= MAX_SPLIT_DEPTH {
			return nil, &splitError{fmt.Sprintf("%s 仍有 %s，已递归切 %d 层仍超限",
				filepath.Base(p), human(float64(size)), depth)}
		}
		logf("  ✂ %s 仍有 %s，对半再切", filepath.Base(p), human(float64(size)))

		dir := filepath.Dir(p)
		tmp := filepath.Join(dir, ".retry-"+strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
		_ = os.RemoveAll(tmp)
		if err := os.MkdirAll(tmp, 0o755); err != nil {
			return nil, &splitError{fmt.Sprintf("建临时目录失败: %v", err)}
		}
		dur, err := mediaDuration(ctx, p)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		base := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		ext := filepath.Ext(p)
		if err := segment(ctx, p, filepath.Join(tmp, base+"_%03d"+ext), dur/2); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		news, _ := filepath.Glob(filepath.Join(tmp, "*"+ext))
		sort.Strings(news)
		if len(news) <= 1 {
			_ = os.RemoveAll(tmp)
			return nil, &splitError{fmt.Sprintf(
				"%s 切不开（%s）——-c copy 只能在关键帧处下刀，该段多半整段没有第二个关键帧。需要重编码才能拆分。",
				filepath.Base(p), human(float64(size)))}
		}

		// 命名成 xxx_003a / xxx_003b：字典序天然排在 _003 与 _004 之间，
		// 整体顺序不会乱，上传顺序也就还是播放顺序。
		//
		// 顺序很重要：必须先把新分片全部改名到位，最后才删原段。
		// 旧实现是 `os.Remove(p)` 之后再逐个 Rename，只要中途有一次 Rename 失败，
		// 原段已经没了、新分片又还散在会被 RemoveAll 掉的临时目录里——两头落空。
		var renamed []string
		for i, n := range news {
			tag := ""
			if i < 26 {
				tag = string(rune('a' + i))
			} else {
				tag = "z" + fmt.Sprint(i-26)
			}
			dst := filepath.Join(dir, base+tag+ext)
			if err := os.Rename(n, dst); err != nil {
				_ = os.RemoveAll(tmp)
				return nil, &splitError{fmt.Sprintf(
					"重命名分片失败: %v（原段 %s 未删除，重跑会重新切割）", err, filepath.Base(p))}
			}
			renamed = append(renamed, dst)
		}
		_ = os.Remove(p)
		_ = os.RemoveAll(tmp)

		more, err := resplitOversized(ctx, renamed, maxBytes, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// splitFile 把 src 切成每段不超过 maxBytes 的若干文件，返回按播放顺序排好的路径。
func splitFile(ctx context.Context, src string, maxBytes int64, baseDir string) ([]string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return nil, &splitError{fmt.Sprintf("无法读取源文件: %v", err)}
	}
	size, mtime := st.Size(), st.ModTime().Unix()

	root := splitRootFor(src, baseDir)
	if names := loadManifest(root, src, size, mtime); names != nil {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = filepath.Join(root, n)
		}
		logf("  ↻ 复用已有分片 %d 段：%s", len(parts), root)
		return parts, nil
	}
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		_ = os.RemoveAll(root) // 上次切到一半的残渣
	}

	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return nil, &splitError{fmt.Sprintf("建分片目录失败: %v", err)}
	}
	if free := freeSpace(filepath.Dir(root)); free >= 0 && free < size+size/20 {
		return nil, &splitError{fmt.Sprintf(
			"%s 只剩 %s，装不下 %s 的分片（可用 --split-dir 指到别的盘）",
			filepath.Dir(root), human(float64(free)), human(float64(size)))}
	}

	dur, err := mediaDuration(ctx, src)
	if err != nil {
		return nil, err
	}
	segS := float64(maxBytes) * SPLIT_SAFETY / (float64(size) / dur)
	logf("  ✂ 超过单文件上限 %s，切段上传（%s / %s，每段约 %s）",
		human(float64(maxBytes)), human(float64(size)), humanDur(dur), humanDur(segS))

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, &splitError{fmt.Sprintf("建分片目录失败: %v", err)}
	}
	ext := filepath.Ext(src)
	stem := strings.TrimSuffix(filepath.Base(src), ext)
	if err := segment(ctx, src, filepath.Join(root, stem+"_%03d"+ext), segS); err != nil {
		return nil, err
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*"+ext))
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, &splitError{fmt.Sprintf("%s: ffmpeg 没产出任何分片", filepath.Base(src))}
	}
	parts, err := resplitOversized(ctx, matches, maxBytes, 0)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = filepath.Base(p)
	}
	manifest := splitManifest{
		Src: resolveKeyPath(src), Size: size, Mtime: mtime,
		MaxBytes: maxBytes, Parts: names, Created: time.Now().Unix(),
	}
	if data, err := json.MarshalIndent(manifest, "", " "); err == nil {
		_ = os.WriteFile(filepath.Join(root, SPLIT_MANIFEST), data, 0o644)
	}

	sizes := make([]string, len(parts))
	for i, p := range parts {
		sizes[i] = human(float64(fileSize(p)))
	}
	logf("  ✂ 切成 %d 段：%s", len(parts), strings.Join(sizes, "、"))
	return parts, nil
}

// cleanupSplit 删掉整个分片目录。名字前缀兜一道底，避免误删别的东西。
func cleanupSplit(root string) {
	if strings.HasPrefix(filepath.Base(root), SPLIT_DIR_PREFIX) {
		_ = os.RemoveAll(root)
	}
}
