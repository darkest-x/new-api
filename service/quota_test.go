package service

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestQuotaDeduction_UserInsufficient 验证用户额度不足时扣减返回
// ErrInsufficientUserQuota，且额度保持不变。
func TestQuotaDeduction_UserInsufficient(t *testing.T) {
	truncate(t)

	const userID = 2001
	const initialQuota = 50
	const consumeQuota = 100 // 超过余额

	seedUser(t, userID, initialQuota)

	err := model.DecreaseUserQuotaSafe(userID, consumeQuota)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInsufficientUserQuota),
		"应返回 ErrInsufficientUserQuota，实际: %v", err)

	assert.EqualValues(t, initialQuota, getUserQuota(t, userID),
		"余额不足时额度应保持不变")
}

// TestQuotaDeduction_UserSufficient 验证用户额度充足时扣减成功且额度正确减少。
func TestQuotaDeduction_UserSufficient(t *testing.T) {
	truncate(t)

	const userID = 2002
	const initialQuota = 200
	const consumeQuota = 80

	seedUser(t, userID, initialQuota)

	err := model.DecreaseUserQuotaSafe(userID, consumeQuota)
	require.NoError(t, err)

	assert.EqualValues(t, initialQuota-consumeQuota, getUserQuota(t, userID),
		"扣减后额度应正确减少")
}

// TestQuotaDeduction_ConcurrentNoOverspend 验证并发扣减不会超扣：
// 多个 goroutine 同时扣减，总扣减不超过初始额度，最终额度不会出现负数。
func TestQuotaDeduction_ConcurrentNoOverspend(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const initialQuota = 50
		const goroutines = 200
		const eachQuota = 1

		seedUserForQuota(t, db, 1, initialQuota)

		var successCount int64
		var failCount int64
		var wg sync.WaitGroup
		wg.Add(goroutines)

		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				<-start // 同时起跑，最大化竞态
				err := model.DecreaseUserQuotaSafe(1, eachQuota)
				if err == nil {
					atomic.AddInt64(&successCount, 1)
				} else if errors.Is(err, model.ErrInsufficientUserQuota) {
					atomic.AddInt64(&failCount, 1)
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		finalQuota := readUserQuotaFromDB(t, db, 1)

		assert.Equal(t, int64(goroutines), successCount+failCount, "所有请求应有明确结果")
		assert.EqualValues(t, int64(initialQuota), successCount, "成功扣减次数应等于初始余额")
		assert.EqualValues(t, int64(goroutines-initialQuota), failCount, "失败次数应为剩余请求")
		assert.EqualValues(t, 0, finalQuota, "最终余额应为 0，绝不超扣为负数")
		assert.GreaterOrEqual(t, finalQuota, int64(0), "额度绝不为负")
	})
}

// TestQuotaDeduction_TokenInsufficient 验证 Token 额度不足时扣减返回
// ErrInsufficientTokenQuota，且额度保持不变。
func TestQuotaDeduction_TokenInsufficient(t *testing.T) {
	truncate(t)

	const userID = 2003
	const tokenID = 2003
	const tokenRemain = 30
	const consumeQuota = 100 // 超过 token 余额

	seedUser(t, userID, 10000) // 用户额度充足
	seedToken(t, tokenID, userID, "sk-quota-token-insufficient", tokenRemain)

	err := model.DecreaseTokenQuotaSafe(tokenID, "sk-quota-token-insufficient", consumeQuota)
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInsufficientTokenQuota),
		"应返回 ErrInsufficientTokenQuota，实际: %v", err)

	assert.EqualValues(t, tokenRemain, getTokenRemainQuota(t, tokenID),
		"余额不足时 token 额度应保持不变")
}

// TestQuotaDeduction_TokenSufficient 验证 Token 额度充足时扣减成功且额度正确减少。
func TestQuotaDeduction_TokenSufficient(t *testing.T) {
	truncate(t)

	const userID = 2004
	const tokenID = 2004
	const tokenRemain = 500
	const consumeQuota = 120

	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "sk-quota-token-sufficient", tokenRemain)

	err := model.DecreaseTokenQuotaSafe(tokenID, "sk-quota-token-sufficient", consumeQuota)
	require.NoError(t, err)

	assert.EqualValues(t, tokenRemain-consumeQuota, getTokenRemainQuota(t, tokenID),
		"扣减后 token 额度应正确减少")
}
