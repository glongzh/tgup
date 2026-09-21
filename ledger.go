package main

// 上传台账：记录已上传文件与每日已用配额，是跨天续传的唯一真相来源。
// JSON 结构与 tgup.py 完全一致，可互换使用同一份 ledger.json。

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ledgerEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Target string `json:"target"`
	MsgID  int64  `json:"msg_id"`
	Ts     int64  `json:"ts"`
}

type Ledger struct {
	mu       sync.Mutex
	path     string
	Uploaded map[string]*ledgerEntry `json:"uploaded"`
	Daily    map[string]int64        `json:"daily"`
}

func openLedger(path string) *Ledger {
	l := &Ledger{
		path:     path,
		Uploaded: map[string]*ledgerEntry{},
		Daily:    map[string]int64{},
	}
	// 台账里存着本机绝对路径和消息 id，目录给 0700、文件给 0600。
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	if data, err := os.ReadFile(path); err == nil {
		text := strings.TrimPrefix(string(data), "\ufeff")
		var parsed Ledger
		if json.Unmarshal([]byte(text), &parsed) == nil {
			if parsed.Uploaded != nil {
				l.Uploaded = parsed.Uploaded
			}
			if parsed.Daily != nil {
				l.Daily = parsed.Daily
			}
		}
	}
	return l
}

// resolveKeyPath 与 Python 的 Path.resolve() 对齐：绝对路径 + 尽量解符号链接。
func resolveKeyPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func ledgerKey(path string, size, mtime int64, target string) string {
	raw := fmt.Sprintf("%s|%s|%d|%d", target, resolveKeyPath(path), size, mtime)
	sum := sha1.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (l *Ledger) IsDone(path string, size, mtime int64, target string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.Uploaded[ledgerKey(path, size, mtime, target)]
	return ok
}

func (l *Ledger) Mark(path string, size, mtime int64, target string, msgID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Uploaded[ledgerKey(path, size, mtime, target)] = &ledgerEntry{
		Path:   resolveKeyPath(path),
		Size:   size,
		Target: target,
		MsgID:  msgID,
		Ts:     time.Now().Unix(),
	}
	l.flushLocked()
}

func todayStr() string { return time.Now().Format("2006-01-02") }

func (l *Ledger) UsedToday() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Daily[todayStr()]
}

func (l *Ledger) AddToday(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := todayStr()
	l.Daily[d] += n
	// 只留最近 60 天
	if len(l.Daily) > 60 {
		keys := make([]string, 0, len(l.Daily))
		for k := range l.Daily {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-60] {
			delete(l.Daily, k)
		}
	}
}

// writeFileAtomic 先写同目录下的 .tmp 再 rename，避免进程被杀时留下半截 JSON。
//
// 临时文件名固定是「目标名 + .tmp」，不做去扩展名处理：
// 早先用 strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"，
// 当目标本身没有扩展名时（例如 --ledger ~/.tgup/ledger），
// 临时名会等于目标名，rename 变成自己改自己，原子性就没了。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// flushLocked 写盘（tmp + rename 原子替换）。调用方必须已持有 l.mu。
func (l *Ledger) flushLocked() {
	data, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(l.path, data)
}

func (l *Ledger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
}

// ---- 续传状态 -----------------------------------------------------------

// stateFile 是 ~/.tgup/state/<hash>.json 的磁盘结构。
// 字段顺序与手写格式一致，因此换用 json.Marshal 后产出的字节完全相同，
// 与 Python 版依旧互通。
type stateFile struct {
	FileID     int64 `json:"file_id"`
	PartSize   int   `json:"part_size"`
	TotalParts int   `json:"total_parts"`
	Done       []int `json:"done"`
	Created    int64 `json:"created"`
}

type uploadState struct {
	mu         sync.Mutex
	path       string
	devnull    bool
	fileID     int64
	partSize   int
	totalParts int
	done       map[int]struct{}
	created    int64
}

func hashStateKey(src string, size, mtime int64) string {
	raw := fmt.Sprintf("%s|%d|%d", resolveKeyPath(src), size, mtime)
	sum := sha1.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func statePathFor(stateDir, src string, size, mtime int64) string {
	return filepath.Join(stateDir, hashStateKey(src, size, mtime)+".json")
}

// discardState 作废某个文件的续传状态，下次从头传。
func discardState(stateDir, src string, size, mtime int64) {
	_ = os.Remove(statePathFor(stateDir, src, size, mtime))
}

// loadOrNewState 读取可用的续传状态；过期/不匹配则新建（见 STATE_TTL 注释）。
func loadOrNewState(stateDir, src string, ps, tp int, size, mtime int64) *uploadState {
	_ = os.MkdirAll(stateDir, 0o700)
	f := statePathFor(stateDir, src, size, mtime)
	now := time.Now().Unix()
	if data, err := os.ReadFile(f); err == nil {
		var d stateFile
		text := strings.TrimPrefix(string(data), "\ufeff")
		if json.Unmarshal([]byte(text), &d) == nil && len(d.Done) > 0 {
			age := now - d.Created
			if age < STATE_TTL && d.PartSize == ps && d.TotalParts == tp {
				logf("  ↻ 续传：已完成 %d/%d 片（%s前）",
					len(d.Done), tp, humanDur(float64(age)))
				done := make(map[int]struct{}, len(d.Done))
				for _, i := range d.Done {
					done[i] = struct{}{}
				}
				return &uploadState{
					path: f, fileID: d.FileID, partSize: ps, totalParts: tp,
					done: done, created: d.Created,
				}
			}
			if age >= STATE_TTL {
				logf("  ↻ 续传状态已放置 %s，服务端多半已丢弃那些分片，从头传",
					humanDur(float64(age)))
			}
		}
	}
	return &uploadState{
		path: f, fileID: randFileID(), partSize: ps, totalParts: tp,
		done: map[int]struct{}{}, created: now,
	}
}

// randFileID 对应 Python 的 random.getrandbits(63) - (1 << 62)。
func randFileID() int64 {
	return rand.Int64()&((1<<63)-1) - (1 << 62)
}

func (s *uploadState) isDone(idx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.done[idx]
	return ok
}

func (s *uploadState) markDone(idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done[idx] = struct{}{}
}

func (s *uploadState) doneCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.done)
}

func (s *uploadState) flush() {
	if s.devnull {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idxs := make([]int, 0, len(s.done))
	for i := range s.done {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	data, err := json.Marshal(stateFile{
		FileID: s.fileID, PartSize: s.partSize, TotalParts: s.totalParts,
		Done: idxs, Created: s.created,
	})
	if err != nil {
		return
	}
	_ = writeFileAtomic(s.path, data)
}

func (s *uploadState) clear() {
	if s.devnull {
		return
	}
	_ = os.Remove(s.path)
}
