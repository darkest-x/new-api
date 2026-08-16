package upstream_guard

import (
	"context"
	"sync"
	"time"
)

// 限速窗口固定为 1 分钟（对齐上游「每分钟 N 次」）。
const windowDuration = 60 * time.Second

// Limiter 是一个「滑动窗口均匀摊派 + 前半段宽松」的限流器：
//   - 每个 60 秒窗口内最多放行 rpm 个请求（严格不超发）；
//   - 窗口内前 burst 个请求立即放行（默认 rpm/2，即「前一半不限制」）；
//   - 超过 burst 后，剩余请求按窗口剩余时间均匀摊派——拉长响应时间、让客户端排队
//     而不是直接报 429；
//   - 窗口名额用尽后等待到下一个窗口。
//
// Wait 阻塞直到放行或 ctx 被取消/超时，语义对齐 golang.org/x/time/rate.Limiter.Wait。
// 刻意不引入第三方依赖，避免交叉编译/离线环境下的 go.mod 增删。
type Limiter struct {
	mu          sync.Mutex
	rpm         int
	burst       int
	windowStart time.Time
	issued      int
}

// NewLimiter 以「每分钟 rpm 次」创建限流器，rpm <= 0 返回 nil（表示不限）。
// burst 为窗口内立即放行的请求数，<= 0 时取 rpm/2。
func NewLimiter(rpm int, burst int) *Limiter {
	if rpm <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = rpm / 2
	}
	if burst < 1 {
		burst = 1
	}
	if burst > rpm {
		burst = rpm
	}
	return &Limiter{
		rpm:         rpm,
		burst:       burst,
		windowStart: time.Now(),
	}
}

// Wait 阻塞直到获得一个放行名额或 ctx 被取消/超时。
// nextAllow 会「预约」并占用名额，因此 Wait 只预约一次，之后睡眠到目标时刻即放行，
// 不能再循环重查（否则每次循环都会再次 issued++ 造成名额虚高、无限拖延）。
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	target, ok := l.nextAllow(time.Now())
	if ok {
		return nil
	}
	for {
		now := time.Now()
		if !now.Before(target) {
			return nil
		}
		timer := time.NewTimer(target.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// nextAllow 在锁内「预约」一个放行名额，返回 (目标时刻, 是否立即放行)。
// 预约即占用名额，保证并发下也不会超发 rpm。
func (l *Limiter) nextAllow(now time.Time) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.windowStart) >= windowDuration {
		l.windowStart = now
		l.issued = 0
	}

	if l.issued >= l.rpm {
		// 本窗口名额已满，等待下一个窗口
		return l.windowStart.Add(windowDuration), false
	}

	// 前半段：burst 个请求立即放行（不限制、只检测计数）
	if l.issued < l.burst {
		l.issued++
		return now, true
	}

	// 后半段：把 60 秒窗口按 rpm 等分，第 issued 个请求排在 (issued+1)*60s/rpm 处。
	// 达到一半后剩余请求即被均匀摊派到剩余时间，客户端只会变慢、不会断开。
	slot := time.Duration(float64(windowDuration) * float64(l.issued+1) / float64(l.rpm))
	target := l.windowStart.Add(slot)
	l.issued++
	if !now.Before(target) {
		return target, true
	}
	return target, false
}
