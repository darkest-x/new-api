package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =====================================================================
// userCheckinWithoutTransaction 测试
//
// 修复前：IncreaseUserQuota 失败时执行 DB.Delete(checkin) 但丢弃错误，
// 回滚失败会留下"已签到但未发放额度"的脏数据，调用方却拿到统一的错误信息。
// 修复后：Delete 错误通过 common.SysError 记录，便于运维定位脏数据。
// =====================================================================

// TestUserCheckinWithoutTransaction_Success 验证成功路径下签到记录与额度均正确写入。
func TestUserCheckinWithoutTransaction_Success(t *testing.T) {
	truncateTables(t)

	user := &User{
		Username:    "checkin_user_ok",
		Password:    "hash",
		DisplayName: "Checkin OK",
		Role:        1,
		Status:      1,
		Quota:       0,
	}
	require.NoError(t, DB.Create(user).Error)

	today := time.Now().Format("2006-01-02")
	checkin := &Checkin{
		UserId:       user.Id,
		CheckinDate:  today,
		QuotaAwarded: 100,
		CreatedAt:    time.Now().Unix(),
	}

	result, err := userCheckinWithoutTransaction(checkin, user.Id, 100)
	require.NoError(t, err)
	require.NotNil(t, result)

	// 签到记录已写入
	var got Checkin
	require.NoError(t, DB.Where("user_id = ?", user.Id).First(&got).Error)
	assert.EqualValues(t, 100, got.QuotaAwarded)

	// 用户额度已增加
	var updated User
	require.NoError(t, DB.First(&updated, user.Id).Error)
	assert.EqualValues(t, 100, updated.Quota)
}

// TestUserCheckinWithoutTransaction_RollbackOnQuotaFail 验证 IncreaseUserQuota 失败时
// 回滚签到记录，且不返回 nil checkin。通过传入负数 quota 触发 IncreaseUserQuota 错误。
func TestUserCheckinWithoutTransaction_RollbackOnQuotaFail(t *testing.T) {
	truncateTables(t)

	user := &User{
		Username:    "checkin_user_rollback",
		Password:    "hash",
		DisplayName: "Checkin Rollback",
		Role:        1,
		Status:      1,
		Quota:       0,
	}
	require.NoError(t, DB.Create(user).Error)

	today := time.Now().Format("2006-01-02")
	checkin := &Checkin{
		UserId:       user.Id,
		CheckinDate:  today,
		QuotaAwarded: 100,
		CreatedAt:    time.Now().Unix(),
	}

	// 传入负数 quota，IncreaseUserQuota 会返回 errors.New("quota 不能为负数！")
	result, err := userCheckinWithoutTransaction(checkin, user.Id, -1)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "签到失败")

	// 签到记录应已被回滚删除
	var count int64
	DB.Model(&Checkin{}).Where("user_id = ?", user.Id).Count(&count)
	assert.Equal(t, int64(0), count, "rollback should delete the checkin record")

	// 用户额度保持不变
	var updated User
	require.NoError(t, DB.First(&updated, user.Id).Error)
	assert.EqualValues(t, 0, updated.Quota)
}

// TestUserCheckinWithoutTransaction_CreateFail 验证 Create 失败时返回错误且不调用 IncreaseUserQuota。
// 通过重复插入触发唯一索引冲突（同一 user_id + checkin_date）。
func TestUserCheckinWithoutTransaction_CreateFail(t *testing.T) {
	truncateTables(t)

	user := &User{
		Username:    "checkin_user_dup",
		Password:    "hash",
		DisplayName: "Checkin Dup",
		Role:        1,
		Status:      1,
		Quota:       0,
	}
	require.NoError(t, DB.Create(user).Error)

	today := time.Now().Format("2006-01-02")
	checkin1 := &Checkin{
		UserId:       user.Id,
		CheckinDate:  today,
		QuotaAwarded: 100,
		CreatedAt:    time.Now().Unix(),
	}
	require.NoError(t, DB.Create(checkin1).Error)

	// 重复插入同一天签到记录应失败
	checkin2 := &Checkin{
		UserId:       user.Id,
		CheckinDate:  today,
		QuotaAwarded: 200,
		CreatedAt:    time.Now().Unix(),
	}
	result, err := userCheckinWithoutTransaction(checkin2, user.Id, 200)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "签到失败")

	// 用户额度保持 0（IncreaseUserQuota 未被调用）
	var updated User
	require.NoError(t, DB.First(&updated, user.Id).Error)
	assert.EqualValues(t, 0, updated.Quota)

	// 原签到记录仍存在且 QuotaAwarded 不变
	var got Checkin
	require.NoError(t, DB.Where("user_id = ?", user.Id).First(&got).Error)
	assert.EqualValues(t, 100, got.QuotaAwarded)
}
