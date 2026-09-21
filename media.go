package main

// 视频探测（ffprobe）与缩略图生成（ffmpeg）、视频 MIME 表。
// 对应 tgup.py 的「媒体」一节。
//
// 这里所有外部命令都接受调用方的 ctx，Ctrl-C 能立刻掐掉正在跑的 ffmpeg/ffprobe。

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Python 标准库 mimetypes 对不少视频容器要么猜错、要么猜不到（如 .ts / .3gp /
// .m2ts / .rmvb）。Telegram 客户端要显示播放按钮，mime 必须是 video/*；
// 这里为脚本自己支持的扩展名建一份专用映射。
var videoMime = map[string]string{
	"mp4": "video/mp4", "m4v": "video/mp4", "mov": "video/quicktime",
	"mkv": "video/x-matroska", "avi": "video/x-msvideo", "webm": "video/webm",
	"flv": "video/x-flv", "wmv": "video/x-ms-wmv",
	"mpg": "video/mpeg", "mpeg": "video/mpeg",
	"ts": "video/mp2t", "m2ts": "video/mp2t",
	"3gp": "video/3gpp", "rmvb": "application/vnd.rn-realmedia-vbr",
	"vob": "video/mpeg",
}

func guessVideoMime(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if m, ok := videoMime[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension("." + ext); m != "" {
		return m
	}
	return "application/octet-stream"
}

func hasTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

type videoMeta struct {
	Duration int
	W        int
	H        int
}

// ffprobeStream 是 ffprobe 输出里我们关心的那部分。
type ffprobeStream struct {
	CodecType string `json:"codec_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

func probeVideo(ctx context.Context, path string) *videoMeta {
	if !hasTool("ffprobe") {
		logf("  ⚠ 未找到 ffprobe，%s 不会带时长/分辨率属性，"+
			"Telegram 大概率显示成文件而非视频。装 ffmpeg 即可", filepath.Base(path))
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json",
		"-select_streams", "v:0",
		"-show_entries", "format=duration:stream=codec_type,width,height",
		path,
	).Output()
	if err != nil {
		// 探测失败意味着用户明确要了 --video 却只能得到一个普通文件，
		// 值得说清楚是哪一步坏了，而不是一句笼统的"探测失败"。
		logf("  ⚠ %s: ffprobe 探测失败（%v）\n"+
			"    → 该文件会以普通文件形式上传：没有播放按钮，也没有缩略图",
			filepath.Base(path), err)
		return nil
	}
	var data struct {
		Streams []ffprobeStream `json:"streams"`
		Format  struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &data) != nil {
		logf("  ⚠ %s: ffprobe 输出解析失败", filepath.Base(path))
		return nil
	}
	var vs *ffprobeStream
	for i := range data.Streams {
		if data.Streams[i].CodecType == "video" {
			vs = &data.Streams[i]
			break
		}
	}
	if vs == nil {
		logf("  ⚠ %s: ffprobe 没找到视频流，按普通文件处理", filepath.Base(path))
		return nil
	}
	dur, _ := strconv.ParseFloat(data.Format.Duration, 64)
	meta := &videoMeta{
		Duration: int(dur),
		W:        vs.Width,
		H:        vs.Height,
	}
	if meta.W == 0 || meta.H == 0 {
		logf("  ⚠ %s: ffprobe 没报出分辨率，Telegram 可能显示成文件而非视频",
			filepath.Base(path))
	}
	return meta
}

// Telegram 对 document 缩略图的要求：JPEG、最长边 320、体积别超过 200KB。
// 超了服务端会直接丢掉 thumb 字段，表现和没传一模一样（黑图），不会报错。
const (
	thumbMaxDim  = 320
	thumbMaxByte = 200 * KB
	thumbMinLuma = 18 // 0-255 平均亮度，低于此认为是黑帧/淡入
)

func meanLuma(ctx context.Context, path string) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg",
		"-v", "quiet", "-i", path,
		"-vf", "scale=1:1,format=gray", "-frames:v", "1", "-f", "rawvideo", "-",
	).Output()
	if err != nil || len(out) == 0 {
		return 0, false
	}
	return int(out[0]), true
}

// grabFrame 抽一帧。-ss 放在 -i 前面是关键帧快速定位；
// thumbnail=100 会在定位点之后的 100 帧里挑「最有代表性」的一帧，
// 天然会避开纯色/淡入帧，比死抠某一帧稳得多。
func grabFrame(ctx context.Context, src, dst string, offset float64, quality int) bool {
	vf := fmt.Sprintf("thumbnail=100,scale=%d:%d:force_original_aspect_ratio=decrease,"+
		"scale=trunc(iw/2)*2:trunc(ih/2)*2", thumbMaxDim, thumbMaxDim) // mjpeg 要求偶数边长
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "quiet", "-y", "-ss", fmt.Sprintf("%.3f", max(offset, 0)),
		"-i", src, "-vf", vf, "-frames:v", "1",
		"-q:v", strconv.Itoa(quality), "-f", "mjpeg", dst)
	return cmd.Run() == nil && fileSize(dst) > 0
}

// makeThumbnail 抽一帧当封面。返回临时 JPEG 路径，调用方负责删。
//
// 不传 thumb 的话 Telegram 会自己抠：MKV 之类的容器它根本抠不出来，
// MP4 则往往抓到第 0 帧——而片头基本都是黑的。这就是「黑色缩略图」的由来。
func makeThumbnail(ctx context.Context, src string, duration int, at *ThumbAt) string {
	if !hasTool("ffmpeg") {
		logf("  ⚠ 未找到 ffmpeg，%s 不会带缩略图（Telegram 多半显示黑图）", filepath.Base(src))
		return ""
	}
	tmp, err := os.CreateTemp("", "tgup-thumb-*.jpg")
	if err != nil {
		return ""
	}
	dst := tmp.Name()
	tmp.Close()

	var offsets []float64
	switch {
	case at != nil:
		if at.Ratio {
			offsets = []float64{float64(duration) * at.Value}
		} else {
			offsets = []float64{at.Value}
		}
	case duration > 0:
		for _, f := range []float64{0.10, 0.30, 0.50, 0.70} {
			offsets = append(offsets, float64(duration)*f)
		}
	default:
		offsets = []float64{5.0, 30.0, 60.0}
	}

	got, chosen := false, offsets[0]
	var luma int
	lumaOK := false
	for _, off := range offsets {
		if !grabFrame(ctx, src, dst, off, 5) {
			continue
		}
		got, chosen = true, off
		luma, lumaOK = meanLuma(ctx, dst)
		if !lumaOK || luma >= thumbMinLuma {
			break // 够亮，就它了
		}
		// 太黑，换个时间点再试；实在都黑就用最后一次的结果
	}
	if got && lumaOK && luma < thumbMinLuma {
		atHint := "，整段视频可能本来就很暗"
		if at != nil {
			atHint = "，换个 --thumb-at 位置试试"
		}
		logf("  ⚠ %s: 取到的帧几乎全黑（亮度 %d/255）%s", filepath.Base(src), luma, atHint)
	}
	if !got {
		logf("  ⚠ %s: 抽帧失败，不带缩略图", filepath.Base(src))
		_ = os.Remove(dst)
		return ""
	}

	for _, q := range []int{12, 20} { // 极少发生，兜个底
		if fileSize(dst) <= thumbMaxByte {
			break
		}
		grabFrame(ctx, src, dst, chosen, q)
	}
	if fileSize(dst) > thumbMaxByte {
		logf("  ⚠ %s: 缩略图 %s 超过 %s，服务端会丢弃，跳过",
			filepath.Base(src), human(float64(fileSize(dst))), human(thumbMaxByte))
		_ = os.Remove(dst)
		return ""
	}
	return dst
}
