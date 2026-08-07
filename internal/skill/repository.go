package skill

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// repository 把 Repository 委托给 SkillRepo，并在这里兑现
// 「用户端只看 enabled，管理端看全量」这条投影规则。
//
// 规则放在这里而不是各 handler 里，是为了让它只有一个出处：
// 散在 handler 里迟早会有一个新加的 handler 忘了过滤 enabled。
type repository struct {
	repo store.SkillRepo
}

// NewRepository 用给定仓储构造 Repository。
func NewRepository(repo store.SkillRepo) Repository { return &repository{repo: repo} }

func (r *repository) List(ctx context.Context) ([]domain.Skill, error) {
	return r.repo.List(ctx, false)
}

func (r *repository) ListAll(ctx context.Context) ([]domain.Skill, error) {
	return r.repo.List(ctx, true)
}

func (r *repository) Get(ctx context.Context, id string) (domain.Skill, error) {
	return r.repo.Get(ctx, id)
}

func (r *repository) Upsert(ctx context.Context, s domain.Skill) (domain.Skill, error) {
	return r.repo.Upsert(ctx, s)
}

func (r *repository) Delete(ctx context.Context, id string) error {
	return r.repo.Delete(ctx, id)
}
