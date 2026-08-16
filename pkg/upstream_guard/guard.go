// Package upstream_guard 提供「渠道+模型」与「渠道+key+模型」两个粒度的
// 限速（排队）与熔断（连续失败后跳过、到时自动恢复）能力。
//
// 状态保存在进程内内存中（sync.Map），面向单实例部署；多实例共享需接入 Redis，
// 属于后续扩展点。所有 API 并发安全。
package upstream_guard

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

const (
	breakerModeFixed = "fixed"
	breakerModeDaily = "daily"
)

// breakerState 记录某个 (渠道,模型) 或 (渠道,key,模型) 的连续失败与熔断状态。
type breakerState struct {
	mu        sync.Mutex
	failures  int
	open      bool
	recoverAt time.Time
}

var (
	modelLimiters sync.Map // "cid|model" -> *Limiter
	modelBreakers sync.Map // "cid|model" -> *breakerState
	keyBreakers   sync.Map // "cid|keyIdx|model" -> *breakerState
)

func limiterKey(channelID, keyIndex int, model string) string {
	return strconv.Itoa(channelID) + "|" + strconv.Itoa(keyIndex) + "|" + model
}

func breakerKey(channelID int, model string) string {
	return strconv.Itoa(channelID) + "|" + model
}

func keyBreakerKey(channelID, keyIndex int, model string) string {
	return strconv.Itoa(channelID) + "|" + strconv.Itoa(keyIndex) + "|" + model
}

// WaitModelRateLimit 在发起上游请求前调用：若该 (渠道,key,模型) 配置了每分钟限额，
// 则在超限时阻塞排队（客户端保持连接、只是变慢），而不是直接返回 429。
// 限速按「每个 key」独立——上游的每分钟限额是 per-key 的，多 key 渠道下各 key 各算各的。
// keyIndex 非多 key 渠道传 0 即可。
// rule.RPM <= 0 表示不限，立即返回。
// 摊派策略：窗口内前 burst（默认 rpm/2）个请求立即放行，之后按剩余时间均匀摊派。
// 排队超过 maxWaitSeconds（规则内可覆盖）或 ctx 被取消时返回错误。
func WaitModelRateLimit(ctx context.Context, channelID int, keyIndex int, model string, rule dto.ChannelModelRateLimit, globalMaxWaitSeconds int) error {
	if rule.RPM <= 0 {
		return nil
	}
	burst := rule.Burst
	if burst <= 0 {
		burst = rule.RPM / 2
	}
	v, loaded := modelLimiters.LoadOrStore(limiterKey(channelID, keyIndex, model), NewLimiter(rule.RPM, burst))
	if !loaded {
		common.SysLog(fmt.Sprintf("[guard] 限速器创建 channel=%d key=%d model=%s rpm=%d burst=%d", channelID, keyIndex, model, rule.RPM, burst))
	}
	limiter, _ := v.(*Limiter)
	if limiter == nil {
		return nil
	}
	// 配置变更（rpm/burst 调整）时重建限速器，让新配置即时生效（无需重启）
	if limiter.rpm != rule.RPM || limiter.burst != burst {
		newLimiter := NewLimiter(rule.RPM, burst)
		modelLimiters.Store(limiterKey(channelID, keyIndex, model), newLimiter)
		limiter = newLimiter
		common.SysLog(fmt.Sprintf("[guard] 限速器重建 channel=%d key=%d model=%s rpm=%d burst=%d", channelID, keyIndex, model, rule.RPM, burst))
	}

	maxWait := globalMaxWaitSeconds
	if rule.MaxWaitSeconds > 0 {
		maxWait = rule.MaxWaitSeconds
	}
	if maxWait <= 0 {
		maxWait = 60
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(maxWait)*time.Second)
	defer cancel()
	start := time.Now()
	if err := limiter.Wait(waitCtx); err != nil {
		common.SysLog(fmt.Sprintf("[guard] 限速排队超时 channel=%d key=%d model=%s rpm=%d waited=%v", channelID, keyIndex, model, rule.RPM, time.Since(start)))
		return fmt.Errorf("模型 %s 在渠道 #%d 触发每分钟限速，排队等待超时（%ds）: %w", model, channelID, maxWait, err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		common.SysLog(fmt.Sprintf("[guard] 限速排队放行 channel=%d key=%d model=%s rpm=%d waited=%v", channelID, keyIndex, model, rule.RPM, elapsed))
	}
	return nil
}

// IsModelOpen 返回 (渠道,模型) 是否处于熔断期。已过恢复时间会惰性关闭并返回 false。
func IsModelOpen(channelID int, model string) bool {
	v, ok := modelBreakers.Load(breakerKey(channelID, model))
	if !ok {
		return false
	}
	st := v.(*breakerState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.open {
		return false
	}
	if !st.recoverAt.IsZero() && time.Now().After(st.recoverAt) {
		st.open = false
		st.failures = 0
		st.recoverAt = time.Time{}
		common.SysLog(fmt.Sprintf("[guard] 熔断恢复 channel=%d model=%s", channelID, model))
		return false
	}
	return true
}

// RecordModelFailure 记录 (渠道,模型) 一次 429/5xx 失败，达到阈值即打开熔断。
// defaultThreshold 为全局默认阈值；defaultCooldown 为全局默认冷却时长（可传秒级，便于测试），
// 规则内 cooldown_minutes/mode/recover_at 可覆盖。
func RecordModelFailure(channelID int, model string, rule dto.ChannelModelCircuitBreaker, defaultThreshold int, defaultCooldown time.Duration) {
	threshold := rule.Threshold
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	if threshold <= 0 {
		return
	}
	v, _ := modelBreakers.LoadOrStore(breakerKey(channelID, model), &breakerState{})
	st := v.(*breakerState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.open {
		return
	}
	st.failures++
	if st.failures >= threshold {
		st.open = true
		st.recoverAt = computeRecoverAt(rule, defaultCooldown)
		common.SysLog(fmt.Sprintf("[guard] 熔断开启 channel=%d model=%s failures=%d threshold=%d recover_at=%s", channelID, model, st.failures, threshold, st.recoverAt.Format("15:04:05")))
	}
}

// RecordModelSuccess 重置 (渠道,模型) 的熔断状态（请求成功即清零）。
func RecordModelSuccess(channelID int, model string) {
	v, ok := modelBreakers.Load(breakerKey(channelID, model))
	if !ok {
		return
	}
	st := v.(*breakerState)
	st.mu.Lock()
	st.failures = 0
	st.open = false
	st.recoverAt = time.Time{}
	st.mu.Unlock()
}

// IsKeyOpen 返回 (渠道,key,模型) 是否处于熔断期。
func IsKeyOpen(channelID, keyIndex int, model string) bool {
	v, ok := keyBreakers.Load(keyBreakerKey(channelID, keyIndex, model))
	if !ok {
		return false
	}
	st := v.(*breakerState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.open {
		return false
	}
	if !st.recoverAt.IsZero() && time.Now().After(st.recoverAt) {
		st.open = false
		st.failures = 0
		st.recoverAt = time.Time{}
		common.SysLog(fmt.Sprintf("[guard] key熔断恢复 channel=%d key=%d model=%s", channelID, keyIndex, model))
		return false
	}
	return true
}

// RecordKeyFailure 记录某个 key 在 (模型) 上的一次失败，达到阈值即熔断该 key+模型。
func RecordKeyFailure(channelID, keyIndex int, model string, threshold int, cooldown time.Duration) {
	if threshold <= 0 {
		return
	}
	v, _ := keyBreakers.LoadOrStore(keyBreakerKey(channelID, keyIndex, model), &breakerState{})
	st := v.(*breakerState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.open {
		return
	}
	st.failures++
	if st.failures >= threshold {
		st.open = true
		if cooldown <= 0 {
			cooldown = 5 * time.Minute
		}
		st.recoverAt = time.Now().Add(cooldown)
		common.SysLog(fmt.Sprintf("[guard] key熔断开启 channel=%d key=%d model=%s failures=%d threshold=%d recover_at=%s", channelID, keyIndex, model, st.failures, threshold, st.recoverAt.Format("15:04:05")))
	}
}

// RecordKeySuccess 重置某个 key 在 (模型) 上的熔断状态。
func RecordKeySuccess(channelID, keyIndex int, model string) {
	v, ok := keyBreakers.Load(keyBreakerKey(channelID, keyIndex, model))
	if !ok {
		return
	}
	st := v.(*breakerState)
	st.mu.Lock()
	st.failures = 0
	st.open = false
	st.recoverAt = time.Time{}
	st.mu.Unlock()
}

// computeRecoverAt 依据规则计算恢复时间：daily 用 "HH:MM"（下次出现），否则 fixed 用冷却时长。
func computeRecoverAt(rule dto.ChannelModelCircuitBreaker, defaultCooldown time.Duration) time.Time {
	now := time.Now()
	if rule.Mode == breakerModeDaily && rule.RecoverAt != "" {
		if t, err := parseDailyRecoverAt(rule.RecoverAt, now); err == nil {
			return t
		}
	}
	cooldown := defaultCooldown
	if rule.CooldownMinutes > 0 {
		cooldown = time.Duration(rule.CooldownMinutes) * time.Minute
	}
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	return now.Add(cooldown)
}

func parseDailyRecoverAt(hhmm string, now time.Time) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid recover_at %q", hhmm)
	}
	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf("invalid recover_at %q", hhmm)
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target, nil
}
