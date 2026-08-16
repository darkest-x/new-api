package upstream_guard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
)

func TestNewLimiterNilForNonPositiveRPM(t *testing.T) {
	if NewLimiter(0, 0) != nil {
		t.Fatal("expected nil limiter for rpm=0")
	}
	if NewLimiter(-1, 0) != nil {
		t.Fatal("expected nil limiter for rpm<0")
	}
}

func TestNewLimiterBurstDefaults(t *testing.T) {
	if l := NewLimiter(40, 0); l.burst != 20 {
		t.Fatalf("expected default burst=rpm/2=20, got %d", l.burst)
	}
	if l := NewLimiter(1, 0); l.burst != 1 {
		t.Fatalf("expected min burst=1, got %d", l.burst)
	}
	if l := NewLimiter(40, 100); l.burst != 40 {
		t.Fatalf("expected burst clamped to rpm=40, got %d", l.burst)
	}
}

func TestLimiterWaitImmediate(t *testing.T) {
	l := NewLimiter(600, 600) // burst=rpm，前 600 个全部立即放行
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("wait should not error: %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("wait should be immediate, took %v", time.Since(start))
	}
}

func TestLimiterWaitBlocksWhenExhausted(t *testing.T) {
	l := NewLimiter(1, 1) // 1/min, burst 1
	if err := l.Wait(context.Background()); err != nil { // 消耗唯一名额
		t.Fatalf("first wait should succeed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected second wait to block until context deadline")
	}
}

func TestLimiterPacingAfterBurst(t *testing.T) {
	// rpm=60, burst=30：前 30 个立即，第 31 个应排到 windowStart + 60s*31/60 = 31s
	l := NewLimiter(60, 30)
	base := time.Now()
	l.windowStart = base
	l.issued = 0
	for i := 0; i < 30; i++ {
		if _, ok := l.nextAllow(base.Add(time.Duration(i) * time.Millisecond)); !ok {
			t.Fatalf("request #%d should be immediate", i)
		}
	}
	target, ok := l.nextAllow(base)
	if ok {
		t.Fatal("request #31 should require waiting, but was immediate")
	}
	expected := base.Add(time.Duration(float64(windowDuration)*31/60))
	if target != expected {
		t.Fatalf("request #31 target = %v, want %v", target, expected)
	}
}

func TestLimiterNeverExceedsRPM(t *testing.T) {
	l := NewLimiter(2, 1) // rpm=2, burst=1
	base := time.Now()
	l.windowStart = base
	l.issued = 0
	if _, ok := l.nextAllow(base); !ok {
		t.Fatal("first request should be immediate")
	}
	target, ok := l.nextAllow(base)
	if ok {
		t.Fatal("second request should require waiting")
	}
	if target != base.Add(windowDuration) {
		t.Fatalf("second target = %v, want %v", target, base.Add(windowDuration))
	}
	target3, ok3 := l.nextAllow(base)
	if ok3 {
		t.Fatal("third request should wait for next window")
	}
	if target3 != base.Add(windowDuration) {
		t.Fatalf("third target = %v, want %v", target3, base.Add(windowDuration))
	}
}

func TestLimiterKeyIsolation(t *testing.T) {
	if limiterKey(1, 0, "m") == limiterKey(1, 1, "m") {
		t.Fatal("different keys must map to different limiter keys")
	}
	if limiterKey(1, 0, "m") == limiterKey(2, 0, "m") {
		t.Fatal("different channels must map to different limiter keys")
	}
	if limiterKey(1, 0, "a") == limiterKey(1, 0, "b") {
		t.Fatal("different models must map to different limiter keys")
	}
}

func TestModelBreakerTripAndReset(t *testing.T) {
	const cid = 900001
	const model = "model-trip"
	rule := dto.ChannelModelCircuitBreaker{Threshold: 3, CooldownMinutes: 5}
	for i := 0; i < 2; i++ {
		RecordModelFailure(cid, model, rule, 0, 0)
		if IsModelOpen(cid, model) {
			t.Fatalf("breaker should not open before threshold, i=%d", i)
		}
	}
	RecordModelFailure(cid, model, rule, 0, 0)
	if !IsModelOpen(cid, model) {
		t.Fatal("breaker should open after threshold")
	}
	RecordModelSuccess(cid, model)
	if IsModelOpen(cid, model) {
		t.Fatal("breaker should close after success")
	}
}

func TestModelBreakerGlobalDefaultThreshold(t *testing.T) {
	const cid = 900011
	const model = "model-default-threshold"
	// 规则未写 threshold，回退到全局默认 2
	for i := 0; i < 2; i++ {
		RecordModelFailure(cid, model, dto.ChannelModelCircuitBreaker{}, 2, 5*time.Minute)
	}
	if !IsModelOpen(cid, model) {
		t.Fatal("breaker should open using global default threshold")
	}
}

func TestKeyBreakerIsolation(t *testing.T) {
	const cid = 900002
	const modelA = "model-a"
	const modelB = "model-b"
	for i := 0; i < 3; i++ {
		RecordKeyFailure(cid, 0, modelA, 3, 5*time.Minute)
	}
	if !IsKeyOpen(cid, 0, modelA) {
		t.Fatal("key0/modelA should be open")
	}
	if IsKeyOpen(cid, 0, modelB) {
		t.Fatal("key0/modelB should not be open (key breaker is per-model)")
	}
	if IsKeyOpen(cid, 1, modelA) {
		t.Fatal("key1/modelA should not be open")
	}
	RecordKeySuccess(cid, 0, modelA)
	if IsKeyOpen(cid, 0, modelA) {
		t.Fatal("key0/modelA should close after success")
	}
}

func TestComputeRecoverAtFixed(t *testing.T) {
	got := computeRecoverAt(dto.ChannelModelCircuitBreaker{CooldownMinutes: 5}, 0)
	if got.Before(time.Now()) {
		t.Fatal("fixed recover time must be in the future")
	}
	// 默认冷却回退
	got2 := computeRecoverAt(dto.ChannelModelCircuitBreaker{}, 7*time.Minute)
	if d := time.Until(got2); d < 6*time.Minute || d > 8*time.Minute {
		t.Fatalf("expected ~7min cooldown fallback, got %v", d)
	}
}

func TestParseDailyRecoverAt(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local)

	got, err := parseDailyRecoverAt("12:30", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.After(now) {
		t.Fatal("recover time must be in the future")
	}
	if got.Hour() != 12 || got.Minute() != 30 {
		t.Fatalf("unexpected recover time: %v", got)
	}

	// 已过的时间点 → 次日
	got2, err := parseDailyRecoverAt("11:00", now)
	if err != nil {
		t.Fatal(err)
	}
	if d := got2.Sub(now); d < 22*time.Hour || d > 24*time.Hour {
		t.Fatalf("expected next-day recovery (~23h), got %v", d)
	}

	if _, err := parseDailyRecoverAt("bad", now); err == nil {
		t.Fatal("expected error for malformed recover_at")
	}
}

func TestModelBreakerAutoRecoversAfterCooldown(t *testing.T) {
	const cid = 900021
	const model = "model-recover"
	rule := dto.ChannelModelCircuitBreaker{Threshold: 2} // 冷却用 defaultCooldown=2s
	RecordModelFailure(cid, model, rule, 2, 2*time.Second)
	RecordModelFailure(cid, model, rule, 2, 2*time.Second)
	if !IsModelOpen(cid, model) {
		t.Fatal("breaker should be open after threshold")
	}
	time.Sleep(2500 * time.Millisecond)
	if IsModelOpen(cid, model) {
		t.Fatal("breaker should auto-recover after cooldown")
	}
}

func TestKeyBreakerAutoRecoversAfterCooldown(t *testing.T) {
	const cid = 900022
	const model = "key-recover"
	for i := 0; i < 2; i++ {
		RecordKeyFailure(cid, 0, model, 2, 2*time.Second)
	}
	if !IsKeyOpen(cid, 0, model) {
		t.Fatal("key breaker should be open after threshold")
	}
	time.Sleep(2500 * time.Millisecond)
	if IsKeyOpen(cid, 0, model) {
		t.Fatal("key breaker should auto-recover after cooldown")
	}
}

func TestBreakerConcurrentAccess(t *testing.T) {
	const cid = 900023
	const model = "model-concurrent"
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordModelFailure(cid, model, dto.ChannelModelCircuitBreaker{Threshold: 5}, 5, time.Second)
			_ = IsModelOpen(cid, model)
			RecordModelSuccess(cid, model)
		}()
	}
	wg.Wait()
	// 并发下不 panic、不卡死即通过；配合 -race 可检测数据竞争
}

// TestLimiterWaitPacingOnce 防回归：Wait 只应预约一次，第 61 个请求（burst=60 之外）
// 应在其摊派时刻（≈30.5s）放行，而不是每次循环重复预约导致无限拖延到窗口翻页。
func TestLimiterWaitPacingOnce(t *testing.T) {
	l := NewLimiter(120, 60) // slot 间隔 0.5s，第 61 个 slot=30.5s
	for i := 0; i < 60; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatalf("request #%d should be immediate: %v", i+1, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	start := time.Now()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("request #61 should be released at its pacing slot, got err: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 40*time.Second || elapsed < 29*time.Second {
		t.Fatalf("request #61 released at %v, want ≈30.5s", elapsed)
	}
}
