package model

import (
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// =====================================================================
// GetUserCheckinStats 测试
//
// 修复前：Count 和 Scan 错误被丢弃，DB 异常时统计显示为 0 但返回 nil error。
// 修复后：Count/Scan 错误必须向上返回，调用方（controller/checkin.go）
// 已有 err != nil 判空处理，兼容无影响。
// =====================================================================

// TestGetUserCheckinStats_Success 验证成功路径下统计数据正确返回。
func TestGetUserCheckinStats_Success(t *testing.T) {
	truncateTables(t)

	user := &User{
		Username:    "stats_user_ok",
		Password:    "hash",
		DisplayName: "Stats OK",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       0,
	}
	require.NoError(t, DB.Create(user).Error)

	// 插入两条签到记录
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	month := time.Now().Format("2006-01")

	require.NoError(t, DB.Create(&Checkin{
		UserId:       user.Id,
		CheckinDate:  yesterday,
		QuotaAwarded: 100,
		CreatedAt:    time.Now().Unix(),
	}).Error)
	require.NoError(t, DB.Create(&Checkin{
		UserId:       user.Id,
		CheckinDate:  today,
		QuotaAwarded: 200,
		CreatedAt:    time.Now().Unix(),
	}).Error)

	stats, err := GetUserCheckinStats(user.Id, month)
	require.NoError(t, err)
	require.NotNil(t, stats)

	assert.Equal(t, int64(2), stats["total_checkins"], "total_checkins should be 2")
	assert.EqualValues(t, int64(300), stats["total_quota"], "total_quota should be 300")
	assert.Equal(t, 2, stats["checkin_count"], "checkin_count should be 2")
	assert.Equal(t, true, stats["checked_in_today"], "checked_in_today should be true")
}

// TestGetUserCheckinStats_NoRecords 验证无签到记录时返回零值统计。
func TestGetUserCheckinStats_NoRecords(t *testing.T) {
	truncateTables(t)

	user := &User{
		Username:    "stats_user_empty",
		Password:    "hash",
		DisplayName: "Stats Empty",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       0,
	}
	require.NoError(t, DB.Create(user).Error)

	month := time.Now().Format("2006-01")
	stats, err := GetUserCheckinStats(user.Id, month)
	require.NoError(t, err)
	require.NotNil(t, stats)

	assert.Equal(t, int64(0), stats["total_checkins"])
	assert.EqualValues(t, int64(0), stats["total_quota"])
	assert.Equal(t, 0, stats["checkin_count"])
	assert.Equal(t, false, stats["checked_in_today"])
}

// TestGetUserCheckinStats_DBError 验证 DB 异常时返回错误。
// 修复前：Count/Scan 错误被丢弃，返回 nil error 和零值统计；
// 修复后：Count/Scan 错误必须向上返回。
func TestGetUserCheckinStats_DBError(t *testing.T) {
	truncateTables(t)

	user := &User{
		Username:    "stats_user_dberr",
		Password:    "hash",
		DisplayName: "Stats DBErr",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       0,
	}
	require.NoError(t, DB.Create(user).Error)

	// 备份原始 DB 并在测试结束后恢复
	origDB := DB
	t.Cleanup(func() {
		DB = origDB
	})

	// 构造一个独立的内存 DB 并立即关闭其底层 sql.DB，模拟 DB 不可用
	closedDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	closedSQLDB, err := closedDB.DB()
	require.NoError(t, err)
	require.NoError(t, closedSQLDB.Close())
	DB = closedDB

	month := time.Now().Format("2006-01")
	stats, err := GetUserCheckinStats(user.Id, month)
	// 修复前：err 为 nil，stats 为零值；修复后：err 应非 nil
	require.Error(t, err)
	assert.Nil(t, stats)
}

// TestGetUserCheckinStats_HasCheckedInTodayDBError 验证 HasCheckedInToday DB 异常时
// GetUserCheckinStats 返回错误，避免 checked_in_today 静默误报为 false。
//
// 通过在独立的 gorm.DB 上注册 Query Callback 拦截 Count 查询（Dest 为 *int64），
// 使 GetUserCheckinRecords（Find）成功而 HasCheckedInToday（Count）失败，
// 从而隔离测试 HasCheckedInToday 的错误传播路径。
//
// 修复前：HasCheckedInToday 错误被丢弃（hasCheckedToday=false），函数继续走
//
//	后续 totalCheckins 的 Count 查询，同样被拦截，返回包装错误
//	"查询签到总数失败: injected count error"。
//
// 修复后：HasCheckedInToday 错误立即返回，错误信息直接为 "injected count error"，
//
//	不会包含 "查询签到总数失败" 包装前缀。
//
// 使用独立的 brokenDB（而非全局 DB）注册 Callback，避免影响其他测试。
// gorm v2 的 processor 没有公开的 Callback 删除方法，因此采用独立 DB 实例 + 全局 swap。
func TestGetUserCheckinStats_HasCheckedInTodayDBError(t *testing.T) {
	// 构造独立的内存 DB，AutoMigrate 所需表，并预置测试数据
	brokenDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, brokenDB.AutoMigrate(&User{}, &Checkin{}))

	user := &User{
		Username:    "stats_user_hti_dberr",
		Password:    "hash",
		DisplayName: "Stats HTI DBErr",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       0,
	}
	require.NoError(t, brokenDB.Create(user).Error)

	// 插入一条签到记录，让 GetUserCheckinRecords（Find）返回非空结果，
	// 确保 HasCheckedInToday 是第一个会触发 Count 拦截的调用。
	today := time.Now().Format("2006-01-02")
	require.NoError(t, brokenDB.Create(&Checkin{
		UserId:       user.Id,
		CheckinDate:  today,
		QuotaAwarded: 100,
		CreatedAt:    time.Now().Unix(),
	}).Error)

	// 注册 Count 拦截 Callback：仅拦截 Dest 为 *int64 的查询（即 Count 调用），
	// Find/Scan 等不受影响，可清晰验证 HasCheckedInToday 的错误传播。
	brokenDB.Callback().Query().Before("gorm:query").Register("fail_count_for_has_checked_today_test", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*int64); ok {
			tx.AddError(errors.New("injected count error"))
		}
	})

	// 切换全局 DB 到 brokenDB，测试结束后恢复
	origDB := DB
	DB = brokenDB
	t.Cleanup(func() {
		DB = origDB
	})

	month := time.Now().Format("2006-01")
	stats, err := GetUserCheckinStats(user.Id, month)
	// 修复前/后均返回错误，但错误来源不同：
	//   修复前：来自后续 totalCheckins Count 的包装错误（"查询签到总数失败: ..."）
	//   修复后：直接来自 HasCheckedInToday
	require.Error(t, err)
	assert.Nil(t, stats)
	assert.Contains(t, err.Error(), "injected count error")
	// 修复后错误应直接来自 HasCheckedInToday，不应是 totalCheckins 的包装错误
	assert.NotContains(t, err.Error(), "查询签到总数失败")
}
