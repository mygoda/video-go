// Package store declares one repository interface per aggregate.
//
// # 为什么按聚合切而不是一个大 DAO
//
// 一个塞满几十个方法的 Store 接口，任何一个 handler 拿到它就等于拿到了全库
// 写权限，也没法在测试里只替换需要的那一小块。按聚合切开之后，
// httpapi.Deps 里列出的就是"这个 handler 到底会碰哪些数据"。
//
// # 分页一律游标，不提供 offset
//
// 资产瀑布流在有新产物插入时，offset 分页会让下一页重复或漏掉条目——
// 而这个平台恰恰在用户浏览列表的同时不断产出新资产，这是常态不是边角情况。
// 因此所有列表接口返回 next_cursor，游标内容对调用方不透明。
//
// 唯一的例外是 status=active 的任务列表：它是 SSE 重连后的对账接口，
// 必须返回全部未终结任务，分页会让对账失去意义。
//
// # 实现
//
// 实现全部落在 internal/store/mysql。本包只声明接口，不 import 任何驱动，
// 保证上层可以在不引入数据库的情况下编译和测试。
package store

import (
	"context"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// Page 是游标分页的结果。NextCursor 为空表示没有下一页。
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// UserRepo 读写用户。
type UserRepo interface {
	Create(ctx context.Context, u domain.User) (domain.User, error)
	GetByID(ctx context.Context, id string) (domain.User, error)
	GetByUsername(ctx context.Context, username string) (domain.User, error)
	UpdatePassword(ctx context.Context, id, passwordHash string) error
	UpdateProfile(ctx context.Context, id string, role domain.Role, status domain.UserStatus, storageQuota int64) (domain.User, error)
	List(ctx context.Context, q, cursor string, limit int) (Page[domain.User], error)
	TouchLastActive(ctx context.Context, id string, at time.Time) error
}

// ProviderRepo 读写供应商配置。
//
// Delete 在该供应商仍被某个 ModelConfig 引用时必须返回 domain.CodeConflict，
// 不能级联删——删掉供应商会让引用它的模型变成一条永远调用失败的死配置。
type ProviderRepo interface {
	List(ctx context.Context) ([]domain.Provider, error)
	Get(ctx context.Context, id string) (domain.Provider, error)
	Upsert(ctx context.Context, p domain.Provider) (domain.Provider, error)
	Delete(ctx context.Context, id string) error
}

// ModelRepo 读写模型配置。
//
// Fingerprint 返回全部启用模型配置的版本指纹，直接作为 GET /api/models 的 ETag。
// 模型池每周在变，因此该接口用 Cache-Control: no-cache（必须回源校验）
// 配 ETag 拿 304，而不是 max-age —— 后者会让用户在模型上下线后
// 拿着过期列表点半天。
type ModelRepo interface {
	List(ctx context.Context, f domain.ModelFilter) ([]domain.ModelConfig, error)
	Get(ctx context.Context, id string) (domain.ModelConfig, error)
	Upsert(ctx context.Context, m domain.ModelConfig) (domain.ModelConfig, error)
	Delete(ctx context.Context, id string) error
	Fingerprint(ctx context.Context) (string, error)
}

// TaskRepo 读写任务。
//
// GetByClientToken 是幂等提交的实现基础：同一用户 24h 内重复提交同一个
// client_token 直接返回已存在的任务（200 而非 201），断网重发不产生两个任务、
// 不重复扣费。
//
// ListActive 返回该用户全部未终结任务（queued + running），**不分页**——
// 见包注释。
//
// UpdateStatus 带 expect 前置状态，是状态机幂等的底座：
// UPDATE ... WHERE status = expect，轮询与 webhook 并发到达时只有一个能生效。
type TaskRepo interface {
	Create(ctx context.Context, t domain.Task) (domain.Task, error)
	Get(ctx context.Context, id string) (domain.Task, error)
	GetByClientToken(ctx context.Context, userID, clientToken string) (domain.Task, bool, error)
	List(ctx context.Context, f domain.TaskFilter) (Page[domain.Task], error)
	ListActive(ctx context.Context, userID string) ([]domain.Task, error)
	UpdateStatus(ctx context.Context, id string, expect, next domain.TaskStatus, terr *domain.TaskError) error
	UpdateProgress(ctx context.Context, id string, progress *float64, queuePosition *int, etaSeconds *float64) error
	SetUpstreamRef(ctx context.Context, id, upstreamRef, rawStatus string) error
	CountRunningByModel(ctx context.Context, userID, modelID string) (int, error)
}

// EventRepo 持久化 SSE 事件，供断线补发。
//
// Append 分配单调递增的 id 并落库。ListAfter 补发某 id 之后的事件。
// DeleteBefore 清理过期事件——事件是补发用的短期缓冲，不是审计日志，
// 无限堆积没有意义。
type EventRepo interface {
	Append(ctx context.Context, ev stream.Event) (int64, error)
	ListAfter(ctx context.Context, userID string, afterID int64, limit int) ([]stream.Event, error)
	DeleteBefore(ctx context.Context, before time.Time) (int64, error)
}

// UploadRepo 读写临时上传对象。
//
// upload 是临时对象：24h 未被任务引用即回收。ListExpired 供清理任务扫描，
// 已被提升为 asset 的（AssetID 非空）不在其列。
type UploadRepo interface {
	Create(ctx context.Context, u domain.Upload) (domain.Upload, error)
	Get(ctx context.Context, id string) (domain.Upload, error)
	MarkPromoted(ctx context.Context, id, assetID string) error
	ListExpired(ctx context.Context, before time.Time, limit int) ([]domain.Upload, error)
	Delete(ctx context.Context, id string) error
}

// AssetRepo 读写资产与血缘边。
//
// Delete 是**软删**：只打删除标记并释放配额，字节留到 admin 的清理任务
// （target=soft_deleted_assets）再回收。硬删会让指向它的血缘边立刻断掉，
// 而血缘是「做同款」与「单卡重跑」的依据，误删一件资产就会连带毁掉
// 一整条创作链的可追溯性。
type AssetRepo interface {
	Create(ctx context.Context, a domain.Asset) (domain.Asset, error)
	Get(ctx context.Context, id string) (domain.Asset, error)
	List(ctx context.Context, userID string, typ domain.AssetType, taskID, cursor string, limit int) (Page[domain.Asset], error)
	ListByTask(ctx context.Context, taskID string) ([]domain.Asset, error)
	Delete(ctx context.Context, id string) error
	AddLineage(ctx context.Context, edges []domain.LineageEdge) error
	Lineage(ctx context.Context, assetID string, dir domain.LineageDirection, depth int) (domain.LineageGraph, error)
	SumBytes(ctx context.Context, userID string) (int64, int, error)
}

// LedgerRepo 读写积分流水。
//
// Append 与 users.credits 的更新必须在同一个事务里，见 internal/billing。
// GetByIdempotencyKey 支撑管理员充值的防重复点击。
type LedgerRepo interface {
	Append(ctx context.Context, e domain.LedgerEntry) (domain.LedgerEntry, error)
	List(ctx context.Context, userID, cursor string, limit int) (Page[domain.LedgerEntry], error)
	GetByIdempotencyKey(ctx context.Context, key string) (domain.LedgerEntry, bool, error)
	Recompute(ctx context.Context, userID string) (domain.Balance, error)
}

// CanvasRepo 读写画布项目、卡片与对话。
//
// ApplyOps 在一个事务里校验 baseRevision、按序落库 ops、推进 revision，
// 冲突时返回 domain.CodeRevisionConflict。有序落库这串 op 也就是
// 「创作过程可回放」的数据基础。
type CanvasRepo interface {
	CreateProject(ctx context.Context, p domain.Project) (domain.Project, error)
	GetProject(ctx context.Context, id string) (domain.Project, error)
	ListProjects(ctx context.Context, userID, cursor string, limit int) (Page[domain.Project], error)
	UpdateProject(ctx context.Context, p domain.Project) (domain.Project, error)
	DeleteProject(ctx context.Context, id string) error
	Snapshot(ctx context.Context, projectID string) (domain.CanvasSnapshot, error)
	ApplyOps(ctx context.Context, projectID string, baseRevision int64, ops []domain.CanvasOp) (int64, error)
	AppendMessage(ctx context.Context, m domain.Message) (domain.Message, error)
}

// SkillRepo 读写 Skill 记录。
type SkillRepo interface {
	List(ctx context.Context, includeDisabled bool) ([]domain.Skill, error)
	Get(ctx context.Context, id string) (domain.Skill, error)
	Upsert(ctx context.Context, s domain.Skill) (domain.Skill, error)
	Delete(ctx context.Context, id string) error
}
