package main

// 单行原地刷新的进度条与全局日志。对应 tgup.py 的 Progress / log。

import (
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

var logMu sync.Mutex

// log 输出一行到 stderr。多条 goroutine 同时写时串行化，避免进度条被撕碎。
func log(msg string) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintln(os.Stderr, msg)
}

func logf(format string, args ...any) {
	log(fmt.Sprintf(format, args...))
}

// fatal 打印并退出，对应 Python 的 SystemExit。
func fatal(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}

type Progress struct {
	mu     sync.Mutex
	name   string
	total  int64
	done   int64
	prefix string
	start  time.Time
	last   time.Time
	dirty  bool
	tty    bool
}

const (
	progressTTYInterval  = 300 * time.Millisecond
	progressFileInterval = 30 * time.Second
)

func newProgress(name string, total, done int64, prefix string) *Progress {
	tty := false
	if fi, err := os.Stderr.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	return &Progress{
		name:   name,
		total:  total,
		done:   done,
		prefix: prefix,
		start:  time.Now(),
		tty:    tty,
	}
}

func terminalCols() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	return 120
}

func (p *Progress) line(sp float64, pct float64, eta float64) string {
	stats := fmt.Sprintf("%5.1f%% %s/%s %s/s ETA %7s",
		pct, human(float64(p.done)), human(float64(p.total)),
		human(sp), humanDur(eta))
	head := "  " + p.prefix
	cols := 120
	if p.tty {
		cols = terminalCols()
	}
	room := cols - 1 - dispWidth(head) - dispWidth(stats) - 1
	if room < 10 { // 终端太窄，名字让位给数字
		return fitDisplay(head+stats, cols-1)
	}
	return head + fitDisplay(p.name, room) + " " + stats
}

// advance 可从多个上传 worker 并发调用。
func (p *Progress) advance(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	now := time.Now()
	final := p.done >= p.total
	interval := progressFileInterval
	if p.tty {
		interval = progressTTYInterval
	}
	if now.Sub(p.last) < interval && !final {
		return
	}
	p.last = now
	el := max(now.Sub(p.start).Seconds(), 1e-6)
	sp := float64(p.done) / el
	pct := 100.0
	if p.total > 0 {
		pct = float64(p.done) / float64(p.total) * 100
	}
	eta := 0.0
	if sp > 0 {
		eta = float64(p.total-p.done) / sp
	}
	line := p.line(sp, pct, eta)
	if p.tty {
		fmt.Fprint(os.Stderr, "\r"+line)
		p.dirty = true
	} else {
		fmt.Fprintln(os.Stderr, trimRightSpace(line))
		p.dirty = false
	}
}

func (p *Progress) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dirty {
		fmt.Fprintln(os.Stderr)
		p.dirty = false
	}
}

func trimRightSpace(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
