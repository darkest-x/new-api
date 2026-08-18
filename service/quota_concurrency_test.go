package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newConcurrentDB 创建支持多连接并发的文件 SQLite DB（WAL 模式）。
// 返回清理函数，调用者需 defer 执行。
//
// 背景：service 包 TestMain 中的内存 SQLite 设置 SetMaxOpenConns(1)，
// 无法真正测试并发竞态。本函数用临时文件 + WAL 模式 + 多连接，
// 让 CAS 的并发正确性可被验证。
func newConcurrentDB(t *testing.T) (*gorm.DB, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "quota_concurrent.db")
	// cache=shared 让多连接共享同一数据库；WAL 模式允许并发读写
	dsn := fmt.Sprintf("file:%s?cache=shared&mode=rwc&_journal_mode=WAL&_busy_timeout=5000", dbPath)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err, "打开并发测试 DB 失败")

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(0) // 不限制连接数，允许真正并发

	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))

	cleanup := func() {
		_ = sqlDB.Close()
		_ = os.RemoveAll(tmpDir)
	}
	return db, cleanup
}

// withConcurrentDB 临时替换 model.DB 为并发 DB，测试结束后恢复。
func withConcurrentDB(t *testing.T, fn func(db *gorm.DB)) {
	t.Helper()
	origDB := model.DB
	db, cleanup := newConcurrentDB(t)
	defer cleanup()
	model.DB = db
	defer func() { model.DB = origDB }()
	fn(db)
}

// seedUserForQuota 在并发 DB 中插入测试用户
func seedUserForQuota(t *testing.T, db *gorm.DB, id int, quota int) {
	t.Helper()
	require.NoError(t, db.Create(&model.User{
		Id:       id,
		Username: fmt.Sprintf("user_%d", id),
		Quota:    int64(quota),
		Status:   common.UserStatusEnabled,
	}).Error)
}

// seedTokenForQuota 在并发 DB 中插入测试令牌
func seedTokenForQuota(t *testing.T, db *gorm.DB, id int, userId int, key string, remain int) {
	t.Helper()
	require.NoError(t, db.Create(&model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "test_token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remain,
		UsedQuota:   0,
	}).Error)
}

// readUserQuotaFromDB 直接从 DB 读取用户额度（绕过缓存）
func readUserQuotaFromDB(t *testing.T, db *gorm.DB, id int) int64 {
	t.Helper()
	var u model.User
	require.NoError(t, db.Select("quota").Where("id = ?", id).First(&u).Error)
	return u.Quota
}

func readTokenQuotaFromDB(t *testing.T, db *gorm.DB, id int) int {
	t.Helper()
	var tk model.Token
	require.NoError(t, db.Select("remain_quota").Where("id = ?", id).First(&tk).Error)
	return tk.RemainQuota
}

// ---------------------------------------------------------------------------
// 测试1：并发100个请求扣同一个用户额度 — 预期不会超扣
// ---------------------------------------------------------------------------

func TestDecreaseUserQuotaSafe_ConcurrentNoOverspend(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const initialQuota = 100
		const goroutines = 100
		const eachQuota = 1 // 每个请求扣 1

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

		assert.EqualValues(t, int64(initialQuota), successCount+failCount, "所有请求应有明确结果")
		assert.EqualValues(t, int64(initialQuota), successCount, "成功扣减次数应等于初始余额")
		assert.EqualValues(t, int64(goroutines-initialQuota), failCount, "失败次数应为剩余请求")
		assert.EqualValues(t, 0, finalQuota, "最终余额应为 0，绝不超扣为负数")
	})
}

// ---------------------------------------------------------------------------
// 测试1b：并发扣 token 额度 — 预期不会超扣
// ---------------------------------------------------------------------------

func TestDecreaseTokenQuotaSafe_ConcurrentNoOverspend(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const initialRemain = 50
		const goroutines = 100
		const eachQuota = 1

		seedUserForQuota(t, db, 1, 1000000) // 用户额度充足，不参与测试
		seedTokenForQuota(t, db, 1, 1, "sk-test-token-key", initialRemain)

		var successCount int64
		var failCount int64
		var wg sync.WaitGroup
		wg.Add(goroutines)

		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				<-start
				err := model.DecreaseTokenQuotaSafe(1, "sk-test-token-key", eachQuota)
				if err == nil {
					atomic.AddInt64(&successCount, 1)
				} else if errors.Is(err, model.ErrInsufficientTokenQuota) {
					atomic.AddInt64(&failCount, 1)
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		finalRemain := readTokenQuotaFromDB(t, db, 1)
		assert.Equal(t, int64(initialRemain), successCount, "成功次数应等于初始 token 余额")
		assert.Equal(t, int64(goroutines-initialRemain), failCount, "失败次数应为剩余请求")
		assert.Equal(t, 0, finalRemain, "最终 token 余额应为 0，绝不超扣为负数")
	})
}

// ---------------------------------------------------------------------------
// 测试2：扣用户额度成功后模拟后续失败 — 预期事务回滚
// ---------------------------------------------------------------------------

// TestPostConsumeQuota_RollbackOnTokenFailure 验证 PostConsumeQuota 在用户额度扣减成功后，
// 令牌扣减失败时会回滚已扣减的用户额度。
//
// 构造场景：用户额度充足，token 额度不足。
// 预期：用户额度扣减成功 → token 扣减返回 ErrInsufficientTokenQuota → 触发回滚 → 用户额度恢复。
func TestPostConsumeQuota_RollbackOnTokenFailure(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const userId = 1
		const tokenId = 1
		const userQuota = 1000
		const tokenRemain = 5 // 不足
		const consumeQuota = 100

		seedUserForQuota(t, db, userId, userQuota)
		seedTokenForQuota(t, db, tokenId, userId, "sk-rollback-test", tokenRemain)

		relayInfo := &relaycommon.RelayInfo{
			UserId:        userId,
			TokenId:       tokenId,
			TokenKey:      "sk-rollback-test",
			BillingSource: BillingSourceWallet,
			// TokenUnlimited = false（默认零值）
		}

		err := PostConsumeQuota(relayInfo, consumeQuota, 0, false)

		// 应返回余额不足错误
		require.Error(t, err, "token 余额不足应返回错误")
		assert.True(t, errors.Is(err, model.ErrInsufficientTokenQuota),
			"错误应为 ErrInsufficientTokenQuota，实际: %v", err)

		// 关键断言：用户额度应已回滚到原值
		finalUserQuota := readUserQuotaFromDB(t, db, userId)
		assert.EqualValues(t, userQuota, finalUserQuota,
			"token 扣减失败后用户额度必须回滚到原值 %d，实际 %d", userQuota, finalUserQuota)

		// token 额度应保持不变（扣减失败）
		finalTokenRemain := readTokenQuotaFromDB(t, db, tokenId)
		assert.Equal(t, tokenRemain, finalTokenRemain, "token 额度应保持原值")
	})
}

// ---------------------------------------------------------------------------
// 测试3：余额不足 — 预期不会产生负数
// ---------------------------------------------------------------------------

func TestDecreaseUserQuotaSafe_InsufficientBalance_NoNegative(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const userId = 1
		const initialQuota = 10
		const overConsume = 50

		seedUserForQuota(t, db, userId, initialQuota)

		err := model.DecreaseUserQuotaSafe(userId, overConsume)
		require.Error(t, err, "余额不足应返回错误")
		assert.True(t, errors.Is(err, model.ErrInsufficientUserQuota),
			"错误应为 ErrInsufficientUserQuota，实际: %v", err)

		finalQuota := readUserQuotaFromDB(t, db, userId)
		assert.EqualValues(t, initialQuota, finalQuota,
			"余额不足时额度应保持不变，绝不产生负数")
		assert.GreaterOrEqual(t, finalQuota, int64(0), "额度绝不为负")
	})
}

func TestDecreaseTokenQuotaSafe_InsufficientBalance_NoNegative(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const tokenId = 1
		const initialRemain = 3
		const overConsume = 20

		seedUserForQuota(t, db, 1, 1000)
		seedTokenForQuota(t, db, tokenId, 1, "sk-insufficient-test", initialRemain)

		err := model.DecreaseTokenQuotaSafe(tokenId, "sk-insufficient-test", overConsume)
		require.Error(t, err, "token 余额不足应返回错误")
		assert.True(t, errors.Is(err, model.ErrInsufficientTokenQuota),
			"错误应为 ErrInsufficientTokenQuota，实际: %v", err)

		finalRemain := readTokenQuotaFromDB(t, db, tokenId)
		assert.Equal(t, initialRemain, finalRemain,
			"token 余额不足时应保持不变，绝不产生负数")
		assert.GreaterOrEqual(t, finalRemain, 0, "token 额度绝不为负")
	})
}

// ---------------------------------------------------------------------------
// 测试4：PreConsumeTokenQuota 通过 service 层调用 — 验证集成路径
// ---------------------------------------------------------------------------

func TestPreConsumeTokenQuota_InsufficientReturnsError(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const tokenId = 1
		const initialRemain = 5

		seedUserForQuota(t, db, 1, 1000)
		seedTokenForQuota(t, db, tokenId, 1, "sk-preconsume-test", initialRemain)

		relayInfo := &relaycommon.RelayInfo{
			UserId:   1,
			TokenId:  tokenId,
			TokenKey: "sk-preconsume-test",
			// TokenUnlimited = false（默认零值）
			// IsPlayground = false（默认零值）
		}

		// 正常扣减
		err := PreConsumeTokenQuota(relayInfo, 3)
		assert.NoError(t, err, "余额足够时应成功")
		assert.EqualValues(t, 2, readTokenQuotaFromDB(t, db, tokenId), "扣减后剩余 2")

		// 余额不足
		err = PreConsumeTokenQuota(relayInfo, 10)
		assert.Error(t, err, "余额不足时应返回错误")
		assert.EqualValues(t, 2, readTokenQuotaFromDB(t, db, tokenId), "失败时额度不变")
	})
}

// ---------------------------------------------------------------------------
// 测试5：无限额度令牌跳过扣减
// ---------------------------------------------------------------------------

func TestPreConsumeTokenQuota_UnlimitedTokenSkipped(t *testing.T) {
	withConcurrentDB(t, func(db *gorm.DB) {
		const tokenId = 1
		const initialRemain = 100

		seedUserForQuota(t, db, 1, 1000)
		seedTokenForQuota(t, db, tokenId, 1, "sk-unlimited-test", initialRemain)

		relayInfo := &relaycommon.RelayInfo{
			UserId:         1,
			TokenId:        tokenId,
			TokenKey:       "sk-unlimited-test",
			TokenUnlimited: true, // 无限额度
		}

		err := PreConsumeTokenQuota(relayInfo, 50)
		assert.NoError(t, err, "无限额度令牌应直接返回成功")
		assert.EqualValues(t, initialRemain, readTokenQuotaFromDB(t, db, tokenId),
			"无限额度令牌额度不应被扣减")
	})
}
