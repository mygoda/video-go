package canvas

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// applier 把 Applier 委托给 CanvasRepo。
//
// 本包不自己写 SQL：乐观锁校验、按序落库、revision 推进这三件事必须在同一个
// 事务里发生（见包注释），而事务边界属于仓储实现。这里保留一层的价值在于
// httpapi 依赖的是 canvas.Applier 这个语义接口，而不是一个数据库仓储——
// 换存储时 handler 不用动。
type applier struct {
	repo store.CanvasRepo
}

// NewApplier 用给定仓储构造 Applier。
func NewApplier(repo store.CanvasRepo) Applier { return &applier{repo: repo} }

func (a *applier) Apply(ctx context.Context, projectID string, baseRevision int64, ops []domain.CanvasOp) (int64, error) {
	return a.repo.ApplyOps(ctx, projectID, baseRevision, ops)
}

func (a *applier) Snapshot(ctx context.Context, projectID string) (domain.CanvasSnapshot, error) {
	return a.repo.Snapshot(ctx, projectID)
}
