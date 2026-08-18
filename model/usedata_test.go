package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =====================================================================
// SaveQuotaDataCache 测试
//
// 修复前：First 和 Create 错误均丢弃。First 失败时 quotaDataDB.Id 为 0，
// 进入 else 分支执行 Create，可能产生重复记录；Create 失败时仍打印"保存成功"。
// 修复后：First 错误（非 ErrRecordNotFound）记录并跳过；Create 错误记录并计数。
// =====================================================================

// helper: 清空 CacheQuotaData 并填充一条
func setupCacheQuotaData(t *testing.T, data *QuotaData) {
	t.Helper()
	CacheQuotaDataLock.Lock()
	defer CacheQuotaDataLock.Unlock()
	CacheQuotaData = make(map[string]*QuotaData)
	key := "test-key"
	CacheQuotaData[key] = data
}

// helper: 清空 CacheQuotaData
func clearCacheQuotaData(t *testing.T) {
	t.Helper()
	CacheQuotaDataLock.Lock()
	defer CacheQuotaDataLock.Unlock()
	CacheQuotaData = make(map[string]*QuotaData)
}

// TestSaveQuotaDataCache_InsertNew 验证缓存数据不存在时正确插入新记录。
func TestSaveQuotaDataCache_InsertNew(t *testing.T) {
	truncateTables(t)
	clearCacheQuotaData(t)

	createdAt := time.Now().Unix()
	createdAt = createdAt - (createdAt % 3600)

	setupCacheQuotaData(t, &QuotaData{
		UserID:    100,
		Username:  "testuser",
		ModelName: "test-model",
		CreatedAt: createdAt,
		Count:     5,
		Quota:     1000,
		TokenUsed: 500,
	})

	SaveQuotaDataCache()

	// 验证 DB 中已插入
	var saved QuotaData
	require.NoError(t, DB.Table("quota_data").Where("user_id = ? AND model_name = ?", 100, "test-model").First(&saved).Error)
	assert.Equal(t, 100, saved.UserID)
	assert.Equal(t, "testuser", saved.Username)
	assert.Equal(t, "test-model", saved.ModelName)
	assert.Equal(t, 5, saved.Count)
	assert.EqualValues(t, 1000, saved.Quota)
	assert.Equal(t, 500, saved.TokenUsed)

	// 验证缓存已清空
	CacheQuotaDataLock.Lock()
	assert.Empty(t, CacheQuotaData)
	CacheQuotaDataLock.Unlock()
}

// TestSaveQuotaDataCache_UpdateExisting 验证缓存数据已存在时正确累加更新。
func TestSaveQuotaDataCache_UpdateExisting(t *testing.T) {
	truncateTables(t)
	clearCacheQuotaData(t)

	createdAt := time.Now().Unix()
	createdAt = createdAt - (createdAt % 3600)

	// 先插入一条已有记录
	require.NoError(t, DB.Table("quota_data").Create(&QuotaData{
		UserID:    200,
		Username:  "existinguser",
		ModelName: "existing-model",
		CreatedAt: createdAt,
		Count:     3,
		Quota:     600,
		TokenUsed: 300,
	}).Error)

	// 缓存中放入同 user_id + model_name + created_at 的数据
	setupCacheQuotaData(t, &QuotaData{
		UserID:    200,
		Username:  "existinguser",
		ModelName: "existing-model",
		CreatedAt: createdAt,
		Count:     2,
		Quota:     400,
		TokenUsed: 200,
	})

	SaveQuotaDataCache()

	// 验证 DB 中数据已累加（3+2=5, 600+400=1000, 300+200=500）
	var updated QuotaData
	require.NoError(t, DB.Table("quota_data").Where("user_id = ? AND model_name = ?", 200, "existing-model").First(&updated).Error)
	assert.Equal(t, 5, updated.Count, "expected count to be accumulated")
	assert.EqualValues(t, 1000, updated.Quota, "expected quota to be accumulated")
	assert.Equal(t, 500, updated.TokenUsed, "expected token_used to be accumulated")

	// 验证没有产生重复记录
	var count int64
	DB.Table("quota_data").Where("user_id = ? AND model_name = ?", 200, "existing-model").Count(&count)
	assert.Equal(t, int64(1), count, "expected no duplicate records")
}

// TestSaveQuotaDataCache_EmptyCache 验证空缓存时不操作 DB 且不报错。
func TestSaveQuotaDataCache_EmptyCache(t *testing.T) {
	truncateTables(t)
	clearCacheQuotaData(t)

	SaveQuotaDataCache()

	// 验证 DB 中无数据
	var count int64
	DB.Table("quota_data").Count(&count)
	assert.Equal(t, int64(0), count, "expected no records for empty cache")
}

// TestSaveQuotaDataCache_MultipleEntries 验证多条缓存数据正确处理。
func TestSaveQuotaDataCache_MultipleEntries(t *testing.T) {
	truncateTables(t)
	clearCacheQuotaData(t)

	createdAt := time.Now().Unix()
	createdAt = createdAt - (createdAt % 3600)

	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaData["key1"] = &QuotaData{
		UserID: 300, Username: "user1", ModelName: "model1", CreatedAt: createdAt,
		Count: 1, Quota: 100, TokenUsed: 50,
	}
	CacheQuotaData["key2"] = &QuotaData{
		UserID: 301, Username: "user2", ModelName: "model2", CreatedAt: createdAt,
		Count: 2, Quota: 200, TokenUsed: 100,
	}
	CacheQuotaDataLock.Unlock()

	SaveQuotaDataCache()

	var count int64
	DB.Table("quota_data").Count(&count)
	assert.Equal(t, int64(2), count, "expected 2 records in DB")
}
