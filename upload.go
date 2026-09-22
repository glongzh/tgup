package main

// 分片上传：随机 file_id、128KB→512KB 自适应分片、并发在途分片、
// 分片级断点续传、逐片重试（FloodWait 顺延 / 其他错误指数退避）。
// 对应 tgup.py 的 upload_file / _send_part。

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// invokerRef 允许在长静默期间把上传用的 invoker 从连接池切回主连接、
// 醒来后再切回去（对应 Python 版 SenderPool.suspend/resume）。
type invokerRef struct {
	mu  sync.RWMutex
	inv tg.Invoker
}

func (r *invokerRef) get() tg.Invoker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.inv
}

func (r *invokerRef) set(i tg.Invoker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inv = i
}

type uploadOpts struct {
	partSize    int // 字节；0 = 自动
	concurrency int
	retries     int
	stateDir    string
	maxParts    int
	resume      bool
	pacer       *Pacer
	pool        *invokerRef
	prefix      string
	name        string // 覆盖 InputFile 的文件名（缩略图用 thumb.jpg）
}

// validatePartSize 校验分片大小是否合法。
//
// ps <= 0 必须放在最前面：后面的 (512*KB)%ps 在 ps==0 时会直接
// 整数除零 panic——一个负责「报错」的函数自己崩掉是最没道理的事。
func validatePartSize(ps int) error {
	if ps <= 0 || ps < KB || ps > MAX_PART_SIZE ||
		ps%KB != 0 || (512*KB)%ps != 0 {
		return fmt.Errorf("part_size 非法: %d（须为 1KB 倍数、不超过 %dKB，且整除 512KB）",
			ps, MAX_PART_SIZE/KB)
	}
	return nil
}

// oversizeError 文件超出协议分片上限。单独成类型：主循环遇到它跳过该文件，
// 而不是像 Python 版那样让整批崩溃。
type oversizeError struct{ msg string }

func (e *oversizeError) Error() string { return e.msg }

func pickPartSize(size int64, maxParts int) (int, error) {
	for ps := MIN_PART_SIZE; ps <= MAX_PART_SIZE; ps *= 2 {
		if (size+int64(ps)-1)/int64(ps) <= int64(maxParts) {
			return ps, nil
		}
	}
	return 0, &oversizeError{fmt.Sprintf(
		"文件过大 %s，超出 %d 片上限（协议硬顶 = %d×%dKB）",
		human(float64(size)), maxParts, maxParts, MAX_PART_SIZE/1024)}
}

func uploadFile(ctx context.Context, src string, size, mtime int64,
	o uploadOpts) (tg.InputFileClass, error) {
	name := filepath.Base(src)
	if o.name != "" {
		name = o.name
	}
	if size == 0 {
		return nil, fmt.Errorf("%s: 空文件", name)
	}

	partSize := o.partSize
	if partSize == 0 {
		var err error
		if partSize, err = pickPartSize(size, o.maxParts); err != nil {
			return nil, err
		}
	}
	if err := validatePartSize(partSize); err != nil {
		return nil, err
	}
	totalParts := int((size + int64(partSize) - 1) / int64(partSize))
	if totalParts > o.maxParts {
		return nil, &oversizeError{fmt.Sprintf("%s: 分片数 %d > %d", name, totalParts, o.maxParts)}
	}

	isBig := size > BIG_FILE_THRESHOLD
	var state *uploadState
	if isBig && o.resume {
		state = loadOrNewState(o.stateDir, src, partSize, totalParts, size, mtime)
	} else {
		// 小文件（<10MB）需要整包 md5，重传代价极低，不做分片续传
		state = &uploadState{devnull: true, fileID: randFileID(),
			partSize: partSize, totalParts: totalParts,
			done: map[int]struct{}{}, created: time.Now().Unix()}
	}

	var md5w *md5digest
	if !isBig {
		md5w = newMD5() // Telegram 小文件协议要求整包 md5
	}

	prog := newProgress(name, size, int64(state.doneCount())*int64(partSize), o.prefix)
	sem := make(chan struct{}, max(o.concurrency, 1))
	var firstErr error
	var errMu sync.Mutex
	var stop atomic.Bool
	var wg sync.WaitGroup
	var flushCounter atomic.Int64

	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		stop.Store(true)
	}

	cleanup := func() {
		state.flush()
		prog.close()
	}

	defer func() {
		if firstErr != nil || ctx.Err() != nil {
			cleanup()
		}
	}()

	f, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	for idx := 0; idx < totalParts; idx++ {
		if stop.Load() {
			break
		}
		// 已传过的分片不占限速配额，直接跳（对应 Python 的 md5-skip 分支）
		if isBig && state.isDone(idx) {
			continue
		}

		n := min(int64(partSize), size-int64(idx)*int64(partSize))
		if o.pacer != nil {
			if err := o.pacer.Acquire(ctx, n); err != nil {
				setErr(err)
				break
			}
			if err := o.pacer.MaybeCooldown(ctx); err != nil {
				setErr(err)
				break
			}
		}

		chunk := make([]byte, n)
		if _, err := f.Seek(int64(idx)*int64(partSize), io.SeekStart); err != nil {
			setErr(err)
			break
		}
		if _, err := io.ReadFull(f, chunk); err != nil {
			setErr(err)
			break
		}
		if md5w != nil {
			md5w.write(chunk)
		}
		if isBig && state.isDone(idx) { // 双重保险，语义与 Python 一致
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			setErr(ctx.Err())
		}
		if stop.Load() {
			break
		}

		wg.Add(1)
		go func(idx int, chunk []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			err := sendPart(ctx, o, idx, chunk, totalParts, state.fileID, isBig,
				func(nb int64) {
					state.markDone(idx)
					prog.advance(nb)
					if flushCounter.Add(1)%20 == 0 {
						state.flush()
					}
				})
			if err != nil {
				setErr(err)
			}
		}(idx, chunk)
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	prog.close()
	state.clear()
	if isBig {
		return &tg.InputFileBig{ID: state.fileID, Parts: totalParts, Name: name}, nil
	}
	return &tg.InputFile{ID: state.fileID, Parts: totalParts, Name: name,
		MD5Checksum: md5w.hex()}, nil
}

// sendPart 发送单个分片，带重试。调用方已持有一个信号量名额，这里负责归还。
func sendPart(ctx context.Context, o uploadOpts, idx int, chunk []byte,
	totalParts int, fileID int64, isBig bool, onDone func(int64)) error {
	for attempt := 0; attempt < o.retries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 每次尝试都重新取 invoker：长静默期间连接池可能被 suspend 换掉
		api := tg.NewClient(o.pool.get())
		t0 := time.Now()
		var err error
		if isBig {
			_, err = api.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{
				FileID: fileID, FilePart: idx, FileTotalParts: totalParts, Bytes: chunk,
			})
		} else {
			_, err = api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: fileID, FilePart: idx, Bytes: chunk,
			})
		}
		if err == nil {
			// 保存成功服务端会回 true；false 视为拒绝
			if pacer := o.pacer; pacer != nil {
				pacer.Observe(int64(len(chunk)), time.Since(t0).Seconds())
			}
			onDone(int64(len(chunk)))
			return nil
		}
		if d, ok := tgerr.AsFloodWait(err); ok {
			logf("\n  ⏳ FloodWait %ds (part %d)", int(d.Seconds())+1, idx)
			if err := sleepCtx(ctx, d+time.Second); err != nil {
				return err
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		bo := float64(min(1<<attempt, 30)) + rand.Float64()
		logf("\n  ⚠ part %d %v，%.1fs 后重试", idx, err, bo)
		if err := sleepCtx(ctx, time.Duration(bo*float64(time.Second))); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("分片 %d 重试 %d 次仍失败", idx, o.retries)
}

// md5digest 简单包装，避免到处 import crypto/md5。
type md5digest struct{ h hash.Hash }

func newMD5() *md5digest { return &md5digest{h: md5.New()} }

func (d *md5digest) write(b []byte) { d.h.Write(b) }

func (d *md5digest) hex() string { return hex.EncodeToString(d.h.Sum(nil)) }
