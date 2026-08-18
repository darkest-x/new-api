package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// =====================================================================
// createRootAccountIfNeed 测试
//
// 修复前：DB.Create(&rootUser) 错误被丢弃，函数始终返回 nil。
// 即使 root 账户创建失败，上层也认为初始化成功，导致系统启动后无 root 账户可用、
// 无法登录管理后台。修复后：Create 失败必须记录 SysError 并返回错误。
// =====================================================================

// TestCreateRootAccountIfNeed_Success 验证空 DB 时创建 root 账户成功。
func TestCreateRootAccountIfNeed_Success(t *testing.T) {
	truncateTables(t)

	err := createRootAccountIfNeed()
	require.NoError(t, err)

	// 验证 root 账户已创建
	var rootUser User
	err = DB.Where("role = ?", common.RoleRootUser).First(&rootUser).Error
	require.NoError(t, err)
	assert.Equal(t, "root", rootUser.Username)
	assert.Equal(t, common.RoleRootUser, rootUser.Role)
	assert.Equal(t, common.UserStatusEnabled, rootUser.Status)
	assert.EqualValues(t, 100000000, rootUser.Quota)
	assert.NotEmpty(t, rootUser.Password, "password should be hashed and stored")
}

// TestCreateRootAccountIfNeed_AlreadyExists 验证已有用户时不再创建 root 账户。
// createRootAccountIfNeed 通过 DB.First(&user) 判断是否已有任意用户，
// 若已存在则直接返回 nil（不进入 if 分支）。
func TestCreateRootAccountIfNeed_AlreadyExists(t *testing.T) {
	truncateTables(t)

	// 预先插入一个普通用户
	existing := &User{
		Username:    "existing_user",
		Password:    "hash",
		DisplayName: "Existing",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Quota:       500,
	}
	require.NoError(t, DB.Create(existing).Error)

	err := createRootAccountIfNeed()
	require.NoError(t, err)

	// 不应创建 root 账户
	var rootCount int64
	DB.Model(&User{}).Where("role = ?", common.RoleRootUser).Count(&rootCount)
	assert.Equal(t, int64(0), rootCount, "should not create root account when a user already exists")
}

// TestCreateRootAccountIfNeed_CreateError 验证 Create 失败时返回错误。
// 通过关闭底层 sql.DB 触发 Create 错误（First 也会失败，但走 if 分支后 Create 必然失败）。
func TestCreateRootAccountIfNeed_CreateError(t *testing.T) {
	truncateTables(t)

	// 备份原始 DB 并在测试结束后恢复
	origDB := DB
	t.Cleanup(func() {
		DB = origDB
	})

	// 构造一个独立的内存 DB 并立即关闭其底层 sql.DB，
	// 这样所有 GORM 调用都会返回 sql.ErrConnDone / "database is closed" 错误。
	closedDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	closedSQLDB, err := closedDB.DB()
	require.NoError(t, err)
	require.NoError(t, closedSQLDB.Close())

	// 替换全局 DB
	DB = closedDB

	err = createRootAccountIfNeed()
	// 修复前：err 为 nil（错误被丢弃）；修复后：err 应非 nil
	require.Error(t, err, "createRootAccountIfNeed should return error when DB.Create fails")
	assert.Contains(t, err.Error(), "create root account failed")

	// 验证原始 DB 中没有 root 账户被创建（使用 origDB 而非已关闭的 DB）
	var rootCount int64
	origDB.Model(&User{}).Where("role = ?", common.RoleRootUser).Count(&rootCount)
	assert.Equal(t, int64(0), rootCount)
}
