// Package skill provides access to Skill records.
//
// Skill 是一条**可插拔的记录**（预置系统提示词 + 默认模型参数），不是硬编码分支。
// 新增一个玩法 = admin 加一条记录，代码零改动——这与「接新模型 = 加一条
// ModelConfig」是同一条设计线：把玩法变成数据，而不是变成 if。
//
// SystemPrompt 原文不下发用户端（只给 system_prompt_ref），管理端才看得到。
package skill

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Repository 读写 Skill 记录。
//
// List 供用户端 GET /api/skills，只返回 enabled 的，按 Order 排序。
// ListAll 供管理端，含已禁用。
type Repository interface {
	List(ctx context.Context) ([]domain.Skill, error)
	ListAll(ctx context.Context) ([]domain.Skill, error)
	Get(ctx context.Context, id string) (domain.Skill, error)
	Upsert(ctx context.Context, s domain.Skill) (domain.Skill, error)
	Delete(ctx context.Context, id string) error
}
