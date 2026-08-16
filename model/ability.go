package model

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/upstream_guard"

	"github.com/samber/lo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Ability struct {
	Group     string  `json:"group" gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Model     string  `json:"model" gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
	ChannelId int     `json:"channel_id" gorm:"primaryKey;autoIncrement:false;index"`
	Enabled   bool    `json:"enabled"`
	Priority  *int64  `json:"priority" gorm:"bigint;default:0;index"`
	Weight    uint    `json:"weight" gorm:"default:0;index"`
	Tag       *string `json:"tag" gorm:"index"`
}

type AbilityWithChannel struct {
	Ability
	ChannelType int `json:"channel_type"`
}

func GetAllEnableAbilityWithChannels() ([]AbilityWithChannel, error) {
	var abilities []AbilityWithChannel
	err := DB.Table("abilities").
		Select("abilities.*, channels.type as channel_type").
		Joins("left join channels on abilities.channel_id = channels.id").
		Where("abilities.enabled = ?", true).
		Scan(&abilities).Error
	return abilities, err
}

func GetGroupEnabledModels(group string) []string {
	var models []string
	// Find distinct models
	// P3-5 修复：Pluck 错误必须记录，避免 DB 异常时静默返回空列表误导调用方。
	if err := DB.Table("abilities").Where(commonGroupCol+" = ? and enabled = ?", group, true).Distinct("model").Pluck("model", &models).Error; err != nil {
		common.SysError(fmt.Sprintf("GetGroupEnabledModels query failed: group=%s, error=%s", group, err.Error()))
	}
	return models
}

func GetEnabledModels() []string {
	var models []string
	// Find distinct models
	// P3-5 修复：Pluck 错误必须记录，避免 DB 异常时静默返回空列表误导调用方。
	if err := DB.Table("abilities").Where("enabled = ?", true).Distinct("model").Pluck("model", &models).Error; err != nil {
		common.SysError(fmt.Sprintf("GetEnabledModels query failed: error=%s", err.Error()))
	}
	return models
}

func GetAllEnableAbilities() []Ability {
	var abilities []Ability
	// P3-5 修复：Find 错误必须记录，避免 DB 异常时静默返回空列表影响下游路由决策。
	if err := DB.Find(&abilities, "enabled = ?", true).Error; err != nil {
		common.SysError(fmt.Sprintf("GetAllEnableAbilities query failed: error=%s", err.Error()))
	}
	return abilities
}

func getPriorities(group string, model string) ([]int, error) {
	var priorities []int
	err := DB.Model(&Ability{}).
		Select("DISTINCT(priority)").
		Where(commonGroupCol+" = ? and model = ? and enabled = ?", group, model, true).
		Order("priority DESC").
		Pluck("priority", &priorities).Error
	if err != nil {
		return nil, err
	}
	return priorities, nil
}

// GetChannel 从 (group, model) 的启用渠道中按权重随机选出一个渠道。
// 处于熔断期的 (渠道,模型) 会被跳过；若 retry 对应优先级的渠道全部熔断，则自动下探到
// 下一优先级，与内存缓存路径（GetRandomSatisfiedChannel）的语义保持一致。
func GetChannel(group string, model string, retry int) (*Channel, error) {
	priorities, err := getPriorities(group, model)
	if err != nil {
		return nil, err
	}
	if len(priorities) == 0 {
		return nil, nil
	}
	for p := retry; p < len(priorities); p++ {
		channel, allBroken, err := getChannelAtPriority(group, model, priorities[p])
		if err != nil {
			return nil, err
		}
		if channel != nil {
			return channel, nil
		}
		if !allBroken {
			return nil, nil
		}
		// 该优先级渠道全部熔断，继续下一优先级
	}
	return nil, nil
}

// getChannelAtPriority 在指定优先级内按权重随机选出一个非熔断渠道。
// 返回 (channel, allBroken, err)：channel 非空表示选中；channel 为空且 allBroken=true
// 表示该优先级存在渠道但全部熔断，需要调用方下探到下一优先级。
func getChannelAtPriority(group string, model string, priority int) (*Channel, bool, error) {
	var abilities []Ability
	err := DB.Where(commonGroupCol+" = ? and model = ? and enabled = ? and priority = ?", group, model, true, priority).
		Order("weight DESC").
		Find(&abilities).Error
	if err != nil {
		return nil, false, err
	}
	if len(abilities) == 0 {
		return nil, false, nil
	}
	// 过滤熔断渠道
	usable := make([]Ability, 0, len(abilities))
	for _, ability_ := range abilities {
		if upstream_guard.IsModelOpen(ability_.ChannelId, model) {
			continue
		}
		usable = append(usable, ability_)
	}
	if len(usable) == 0 {
		return nil, true, nil
	}
	abilities = usable

	// 按权重随机选一个
	weightSum := uint(0)
	for _, ability_ := range abilities {
		weightSum += ability_.Weight + 10
	}
	weight := common.GetRandomInt(int(weightSum))
	channel := Channel{}
	for _, ability_ := range abilities {
		weight -= int(ability_.Weight) + 10
		if weight <= 0 {
			channel.Id = ability_.ChannelId
			break
		}
	}
	err = DB.First(&channel, "id = ?", channel.Id).Error
	return &channel, false, err
}

func (channel *Channel) AddAbilities(tx *gorm.DB) error {
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilitySet := make(map[string]struct{})
	abilities := make([]Ability, 0, len(models_))
	for _, model := range models_ {
		for _, group := range groups_ {
			key := group + "|" + model
			if _, exists := abilitySet[key]; exists {
				continue
			}
			abilitySet[key] = struct{}{}
			ability := Ability{
				Group:     group,
				Model:     model,
				ChannelId: channel.Id,
				Enabled:   channel.Status == common.ChannelStatusEnabled,
				Priority:  channel.Priority,
				Weight:    uint(channel.GetWeight()),
				Tag:       channel.Tag,
			}
			abilities = append(abilities, ability)
		}
	}
	if len(abilities) == 0 {
		return nil
	}
	// choose DB or provided tx
	useDB := DB
	if tx != nil {
		useDB = tx
	}
	for _, chunk := range lo.Chunk(abilities, 50) {
		err := useDB.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error
		if err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) DeleteAbilities() error {
	return DB.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
}

// UpdateAbilities updates abilities of this channel.
// Make sure the channel is completed before calling this function.
func (channel *Channel) UpdateAbilities(tx *gorm.DB) error {
	isNewTx := false
	// 如果没有传入事务，创建新的事务
	if tx == nil {
		tx = DB.Begin()
		if tx.Error != nil {
			return tx.Error
		}
		isNewTx = true
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
			}
		}()
	}

	// First delete all abilities of this channel
	err := tx.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
	if err != nil {
		if isNewTx {
			tx.Rollback()
		}
		return err
	}

	// Then add new abilities
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilitySet := make(map[string]struct{})
	abilities := make([]Ability, 0, len(models_))
	for _, model := range models_ {
		for _, group := range groups_ {
			key := group + "|" + model
			if _, exists := abilitySet[key]; exists {
				continue
			}
			abilitySet[key] = struct{}{}
			ability := Ability{
				Group:     group,
				Model:     model,
				ChannelId: channel.Id,
				Enabled:   channel.Status == common.ChannelStatusEnabled,
				Priority:  channel.Priority,
				Weight:    uint(channel.GetWeight()),
				Tag:       channel.Tag,
			}
			abilities = append(abilities, ability)
		}
	}

	if len(abilities) > 0 {
		for _, chunk := range lo.Chunk(abilities, 50) {
			err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error
			if err != nil {
				if isNewTx {
					tx.Rollback()
				}
				return err
			}
		}
	}

	// 如果是新创建的事务，需要提交
	if isNewTx {
		return tx.Commit().Error
	}

	return nil
}

func UpdateAbilityStatus(channelId int, status bool) error {
	return DB.Model(&Ability{}).Where("channel_id = ?", channelId).Select("enabled").Update("enabled", status).Error
}

func UpdateAbilityStatusByTag(tag string, status bool) error {
	return DB.Model(&Ability{}).Where("tag = ?", tag).Select("enabled").Update("enabled", status).Error
}

func UpdateAbilityByTag(tag string, newTag *string, priority *int64, weight *uint) error {
	ability := Ability{}
	if newTag != nil {
		ability.Tag = newTag
	}
	if priority != nil {
		ability.Priority = priority
	}
	if weight != nil {
		ability.Weight = *weight
	}
	return DB.Model(&Ability{}).Where("tag = ?", tag).Updates(ability).Error
}

var fixLock = sync.Mutex{}

// FixAbility 重建 abilities 表并刷新内存缓存。
// P4-5 修复：原签名 (int, int, error) 中 error 仅覆盖 DB 级失败（truncate/query），
// 而 InitChannelCache 失败被静默 SysError，调用方无法区分 "abilities 已写入但缓存未刷新" 与 "DB 写入失败"。
// 现改为返回独立的 cacheRefreshErr，调用方可根据上下文决定是否告警/重试。
// 返回值语义：
//   - successCount: 成功写入 abilities 的渠道数量
//   - failCount:    写入 abilities 失败的渠道数量
//   - cacheRefreshErr: DB 写入完成后的 InitChannelCache 失败（非致命，abilities 表已更新）
//   - err:          DB 级失败（truncate/query），整个流程未执行或中途 DB 错误
func FixAbility() (successCount int, failCount int, cacheRefreshErr error, err error) {
	lock := fixLock.TryLock()
	if !lock {
		return 0, 0, nil, errors.New("已经有一个修复任务在运行中，请稍后再试")
	}
	defer fixLock.Unlock()

	// truncate abilities table
	if common.UsingSQLite {
		err = DB.Exec("DELETE FROM abilities").Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Delete abilities failed: %s", err.Error()))
			return 0, 0, nil, err
		}
	} else {
		err = DB.Exec("TRUNCATE TABLE abilities").Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Truncate abilities failed: %s", err.Error()))
			return 0, 0, nil, err
		}
	}
	var channels []*Channel
	// Find all channels
	err = DB.Model(&Channel{}).Find(&channels).Error
	if err != nil {
		return 0, 0, nil, err
	}
	if len(channels) == 0 {
		// 即使无 channel 也需刷新缓存，保持 cache 与 DB 一致
		if cacheErr := InitChannelCache(); cacheErr != nil {
			common.SysError(fmt.Sprintf(
				"FixAbility: InitChannelCache failed after empty ability rebuild: cache_refresh_required=true, channel_count=0, err=%v",
				cacheErr,
			))
			return 0, 0, cacheErr, nil
		}
		return 0, 0, nil, nil
	}
	for _, chunk := range lo.Chunk(channels, 50) {
		ids := lo.Map(chunk, func(c *Channel, _ int) int { return c.Id })
		// Delete all abilities of this channel
		err = DB.Where("channel_id IN ?", ids).Delete(&Ability{}).Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Delete abilities failed: %s", err.Error()))
			failCount += len(chunk)
			continue
		}
		// Then add new abilities
		for _, channel := range chunk {
			err = channel.AddAbilities(nil)
			if err != nil {
				common.SysLog(fmt.Sprintf("Add abilities for channel %d failed: %s", channel.Id, err.Error()))
				failCount++
			} else {
				successCount++
			}
		}
	}
	if cacheErr := InitChannelCache(); cacheErr != nil {
		// P4-5: abilities 表已成功更新，但内存缓存刷新失败。
		// 调用方需感知此状态以便决定是否告警/重试，但不应将整个 FixAbility 视为 DB 失败。
		common.SysError(fmt.Sprintf(
			"FixAbility: InitChannelCache failed after ability update: cache_refresh_required=true, channel_count=%d, success_count=%d, fail_count=%d, err=%v",
			len(channels), successCount, failCount, cacheErr,
		))
		return successCount, failCount, cacheErr, nil
	}
	return successCount, failCount, nil, nil
}
