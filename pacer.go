package main

// 上行整形器。每发一个分片前调用 Acquire()，它依次检查：
//
//	时间窗 → 日配额 → 占空比 → 令牌桶
//
// 任何一项不满足就在这里睡，调用方不需要关心。
//
// 另外对外暴露 OnPause / OnWake 两个钩子：进入长静默前、恢复传输后各调一次，
// 上层用它在静默期间收掉多余的上传连接（对应 Python 版的 SenderPool.suspend/resume）。
// 对应 tgup.py 的 Pacer 一节。

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const longPauseS = 120.0 // 超过这个时长就认为值得把连接收掉再重建

type dailyCapError struct{ Used, Cap int64 }

func (e *dailyCapError) Error() string {
	return fmt.Sprintf("今日配额已用 %s / %s", human(float64(e.Used)), human(float64(e.Cap)))
}

type throughputSample struct {
	t  time.Time
	n  int64
	dt float64
}

type pauseSpan struct{ start, end time.Time }

type Pacer struct {
	// 配置（构造后只读）
	burstS, restS float64
	windows       []TimeWindow
	dailyCap      int64
	ledger        *Ledger
	onCap         string
	adaptive      bool
	floor         float64
	probeS        float64
	cooldownS     float64
	jitter        float64

	// rate/minRate 会在自适应降速时变化，只在主循环 goroutine 改写
	rate     float64
	baseRate float64
	minRate  float64

	// muTok 保护令牌桶；与 Python 版一样，等待期间持有锁，
	// 使多个并发分片在桶前天然排队。
	muTok   sync.Mutex
	tokens  float64
	lastRef time.Time

	// muSt 保护样本/停顿记录（Observe 从 worker goroutine 调用）
	muSt        sync.Mutex
	samples     []throughputSample
	pauses      []pauseSpan
	pausedTotal float64
	sentBytes   int64

	// 以下仅主循环 goroutine 使用
	burstStarted  time.Time
	hasBurst      bool
	peak          float64 // 本次会话实际达到过的吞吐峰值
	warnedCap     bool
	lastDownshift time.Time
	idle          bool

	OnPause func(ctx context.Context) error
	OnWake  func(ctx context.Context) error
}

func newPacer(rate float64, burstS, restS float64, windows []TimeWindow,
	dailyCap int64, ledger *Ledger, onCap string, adaptive bool,
	floor, probeS, cooldownS, minRate float64) *Pacer {

	p := &Pacer{
		rate: rate, baseRate: rate, minRate: minRate,
		burstS: burstS, restS: restS, windows: windows,
		dailyCap: dailyCap, ledger: ledger, onCap: onCap,
		adaptive: adaptive && rate > 0,
		floor:    floor, probeS: probeS, cooldownS: cooldownS,
		jitter: 0.15,
	}
	if p.minRate <= 0 && rate > 0 {
		p.minRate = rate * 0.2
	}
	p.lastRef = time.Now()
	return p
}

// ---- 对外 ---------------------------------------------------------------

// Acquire 阻塞直到可以发送 n 字节。
func (p *Pacer) Acquire(ctx context.Context, n int64) error {
	if err := p.waitWindow(ctx); err != nil {
		return err
	}
	if err := p.checkDaily(n); err != nil {
		return err
	}
	if err := p.dutyCycle(ctx); err != nil {
		return err
	}
	p.muSt.Lock()
	wasIdle := p.idle
	p.idle = false
	p.muSt.Unlock()
	if wasIdle && p.OnWake != nil {
		if err := p.OnWake(ctx); err != nil {
			return err
		}
	}
	if p.rate > 0 {
		if err := p.takeTokens(ctx, n); err != nil {
			return err
		}
	}
	p.muSt.Lock()
	p.sentBytes += n
	p.muSt.Unlock()
	if p.ledger != nil {
		p.ledger.AddToday(n)
	}
	return nil
}

// Observe 分片发送成功后回调，用于实测吞吐。
func (p *Pacer) Observe(nbytes int64, dt float64) {
	if !p.adaptive || dt <= 0 {
		return
	}
	now := time.Now()
	p.muSt.Lock()
	defer p.muSt.Unlock()
	p.samples = append(p.samples, throughputSample{now, nbytes, dt})
	cutoff := now.Add(-time.Duration(p.probeS * float64(time.Second)))
	i := 0
	for i < len(p.samples) && p.samples[i].t.Before(cutoff) {
		i++
	}
	if i > 0 {
		p.samples = append(p.samples[:0], p.samples[i:]...)
	}
}

func (p *Pacer) pausedWithin(a, b time.Time) float64 {
	var total float64
	for _, s := range p.pauses {
		lo, hi := s.start, s.end
		if b.Before(lo) || a.After(hi) {
			continue
		}
		lo2, hi2 := maxTime(lo, a), minTime(hi, b)
		if hi2.After(lo2) {
			total += hi2.Sub(lo2).Seconds()
		}
	}
	return total
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// measuredThroughput 实测吞吐（字节/秒）。分母要扣掉限速休眠、占空比静默、
// 时间窗等待这些本脚本自己制造的空窗，否则会把自己的节流误判成对端限速。
func (p *Pacer) measuredThroughput() (float64, bool) {
	p.muSt.Lock()
	defer p.muSt.Unlock()
	if len(p.samples) < 12 {
		return 0, false
	}
	t0, t1 := p.samples[0].t, p.samples[len(p.samples)-1].t
	span := t1.Sub(t0).Seconds() - p.pausedWithin(t0, t1)
	if span < p.probeS*0.3 {
		return 0, false
	}
	var sum int64
	for _, s := range p.samples {
		sum += s.n
	}
	return float64(sum) / span, true
}

// checkDegraded 返回 实测吞吐 / 参考基准。参考基准取「本次实际达到过的峰值」与
// 「配置上限」中较小的那个。
//
// 这是关键：旧实现直接拿实测值除以 --rate，一旦 rate 设得比链路实际能跑的还高
// （很常见），比值永远低于阈值，就会无限误报「被限速」并自我降速+冷却，越跑越慢。
// 以实际峰值为基准才能只在真正出现「先快后慢」的退化时才触发。
func (p *Pacer) checkDegraded() (float64, bool) {
	if !p.adaptive {
		return 0, false
	}
	tp, ok := p.measuredThroughput()
	if !ok {
		return 0, false
	}
	// 峰值缓慢衰减，避免开头一次偶然的高值把基准永久钉死
	p.peak = max(p.peak*0.995, tp)
	ref := p.peak
	if p.rate > 0 {
		ref = min(p.peak, p.rate)
	}
	if ref <= 0 {
		return 0, false
	}

	// 顺带提醒一次：限速根本没生效，瓶颈在别处
	p.muSt.Lock()
	nSamples := len(p.samples)
	p.muSt.Unlock()
	if !p.warnedCap && p.rate > 0 && p.peak < p.rate*0.6 && nSamples >= 30 {
		p.warnedCap = true
		logf("\n  ℹ 实测上传峰值约 %s/s，远低于 --rate 设定的 %s/s——"+
			"限速器并未生效，瓶颈在链路或连接数。", human(p.peak), human(p.rate))
		logf("    可尝试提高 --connections（当前每条 TCP 连接的吞吐受 RTT 和丢包制约），" +
			"或把 --rate 调到接近实测值以免误判。")
	}
	return tp / ref, true
}

// MaybeCooldown 确认出现真实退化（相对自身峰值）才冷却降速。
func (p *Pacer) MaybeCooldown(ctx context.Context) error {
	ratio, ok := p.checkDegraded()
	if !ok || ratio >= p.floor {
		if ok && ratio > 0.85 && p.rate > 0 && p.rate < p.baseRate &&
			time.Since(p.lastDownshift) > 30*time.Minute {
			p.rate = min(p.baseRate, p.rate*1.25)
			p.lastDownshift = time.Now()
			logf("\n  ↗ 吞吐恢复，目标速率上调至 %s/s", human(p.rate))
		}
		return nil
	}
	if time.Since(p.lastDownshift).Seconds() < p.cooldownS {
		return nil // 刚降过速，别连环触发
	}
	old := p.rate
	if p.rate > 0 {
		p.rate = max(p.minRate, p.rate*0.6)
	}
	p.lastDownshift = time.Now()
	prePeak := p.peak
	p.peak *= 0.8 // 基准跟着下调，否则冷却后立刻又判定为退化
	p.muSt.Lock()
	p.samples = nil
	p.muSt.Unlock()
	logf("\n  ⚠ 吞吐降至自身峰值的 %.0f%%（峰值 %s/s），疑似被限速",
		ratio*100, human(prePeak))
	if old > 0 {
		logf("    速率 %s/s → %s/s，冷却 %s", human(old), human(p.rate), humanDur(p.cooldownS))
	}
	return p.Sleep(ctx, p.cooldownS)
}

// InterFilePause 文件之间的随机间隔。
func (p *Pacer) InterFilePause(ctx context.Context, lo, hi float64) error {
	if hi <= 0 {
		return nil
	}
	d := lo + rand.Float64()*(hi-lo)
	logf("  ⏸ 文件间隔 %s", humanDur(d))
	return p.Sleep(ctx, d)
}

// Sleep 带 long-pause 钩子的睡眠；睡眠时长计入 pausedTotal，
// 供实测吞吐扣掉「自己造成的空窗」。
func (p *Pacer) Sleep(ctx context.Context, s float64) error {
	if s >= longPauseS {
		p.muSt.Lock()
		notIdle := !p.idle
		p.idle = true
		p.muSt.Unlock()
		if notIdle && p.OnPause != nil {
			if err := p.OnPause(ctx); err != nil {
				return err
			}
		}
	}
	t0 := time.Now()
	if err := sleepCtx(ctx, time.Duration(s*float64(time.Second))); err != nil {
		return err
	}
	t1 := time.Now()
	p.muSt.Lock()
	p.pauses = append(p.pauses, pauseSpan{t0, t1})
	if len(p.pauses) > 500 {
		p.pauses = p.pauses[len(p.pauses)-500:]
	}
	p.pausedTotal += t1.Sub(t0).Seconds()
	p.muSt.Unlock()
	return nil
}

func (p *Pacer) PausedTotal() float64 {
	p.muSt.Lock()
	defer p.muSt.Unlock()
	return p.pausedTotal
}

func (p *Pacer) SentBytes() int64 {
	p.muSt.Lock()
	defer p.muSt.Unlock()
	return p.sentBytes
}

// ---- 内部 ---------------------------------------------------------------

func (p *Pacer) jit(v float64) float64 { return jitterValue(v, p.jitter) }

func (p *Pacer) waitWindow(ctx context.Context) error {
	if len(p.windows) == 0 {
		return nil
	}
	for {
		now := time.Now()
		mod := now.Hour()*60 + now.Minute()
		for _, w := range p.windows {
			if inWindow(mod, w) {
				return nil
			}
		}
		nxt := p.nextOpen(now)
		wait := nxt.Sub(now).Seconds()
		logf("\n  🌙 当前不在允许时段，休眠至 %s（%s）",
			nxt.Format("01-02 15:04"), humanDur(wait))
		p.hasBurst = false
		if err := p.Sleep(ctx, min(wait, 300)); err != nil { // 分段睡，便于 Ctrl-C
			return err
		}
	}
}

func (p *Pacer) nextOpen(now time.Time) time.Time {
	var best time.Time
	for day := 0; day <= 1; day++ {
		base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
			AddDate(0, 0, day)
		for _, w := range p.windows {
			dt := base.Add(time.Duration(w.Start) * time.Minute)
			if dt.After(now) && (best.IsZero() || dt.Before(best)) {
				best = dt
			}
		}
	}
	if best.IsZero() {
		return now.Add(5 * time.Minute)
	}
	return best
}

func (p *Pacer) checkDaily(n int64) error {
	if p.dailyCap <= 0 || p.ledger == nil {
		return nil
	}
	used := p.ledger.UsedToday()
	if used+n > p.dailyCap {
		return &dailyCapError{Used: used, Cap: p.dailyCap}
	}
	return nil
}

func (p *Pacer) dutyCycle(ctx context.Context) error {
	if p.burstS <= 0 || p.restS <= 0 {
		return nil
	}
	now := time.Now()
	if !p.hasBurst {
		p.hasBurst = true
		p.burstStarted = now
		return nil
	}
	if now.Sub(p.burstStarted).Seconds() >= p.jit(p.burstS) {
		rest := p.jit(p.restS)
		logf("\n  💤 已连续上传 %s，静默 %s", humanDur(now.Sub(p.burstStarted).Seconds()), humanDur(rest))
		if err := p.Sleep(ctx, rest); err != nil {
			return err
		}
		p.hasBurst = true
		p.burstStarted = time.Now()
	}
	return nil
}

// takeTokens 令牌桶：桶深 = 1 秒流量，且至少装得下一个分片；空桶起步，
// 避免开场满速突发。等待期间持有 muTok，令所有并发分片在此排队。
func (p *Pacer) takeTokens(ctx context.Context, n int64) error {
	p.muTok.Lock()
	defer p.muTok.Unlock()
	rate := p.rate
	capacity := max(rate, float64(n))
	for {
		now := time.Now()
		p.tokens = min(capacity, p.tokens+now.Sub(p.lastRef).Seconds()*rate)
		p.lastRef = now
		if p.tokens >= float64(n) {
			p.tokens -= float64(n)
			return nil
		}
		need := (float64(n) - p.tokens) / rate
		if err := sleepCtx(ctx, time.Duration(need*float64(time.Second))); err != nil {
			return err
		}
	}
}
