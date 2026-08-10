package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// ── 用户 ────────────────────────────────────────────────────────

func (s *server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	cursor, limit := pagination(r)
	page, err := s.deps.Users.List(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), cursor, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pageOf(page.Items, page.NextCursor))
}

func (s *server) handleAdminGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "userId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	u, err := s.deps.Users.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleAdminUpdateUser 改角色、状态与存储配额。
//
// 三个字段都是指针：PATCH 语义下"没传"和"传了零值"必须区分开，
// 否则一次只想改角色的请求会顺手把配额清成 0。
func (s *server) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "userId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Role              *domain.Role       `json:"role"`
		Status            *domain.UserStatus `json:"status"`
		StorageQuotaBytes *int64             `json:"storage_quota_bytes"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	cur, err := s.deps.Users.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var fields []domain.FieldError
	role := cur.Role
	if req.Role != nil {
		if *req.Role != domain.RoleUser && *req.Role != domain.RoleAdmin {
			fields = append(fields, domain.FieldError{Key: "role", Message: "取值只能是 user 或 admin"})
		}
		role = *req.Role
	}
	status := cur.Status
	if req.Status != nil {
		if *req.Status != domain.UserStatusActive && *req.Status != domain.UserStatusDisabled {
			fields = append(fields, domain.FieldError{Key: "status", Message: "取值只能是 active 或 disabled"})
		}
		status = *req.Status
	}
	quota := cur.StorageQuota
	if req.StorageQuotaBytes != nil {
		if *req.StorageQuotaBytes < 0 {
			fields = append(fields, domain.FieldError{Key: "storage_quota_bytes", Message: "不能为负"})
		}
		quota = *req.StorageQuotaBytes
	}
	if len(fields) > 0 {
		writeError(w, r, errFields(fields, "参数校验未通过"))
		return
	}

	// 最后一个管理员不能把自己降级或停用：真降下去就没人能再改回来了，
	// 只能进数据库手工救。
	if cur.Role == domain.RoleAdmin && (role != domain.RoleAdmin || status != domain.UserStatusActive) {
		if caller, ok := IdentityFrom(r.Context()); ok && caller.UserID == id {
			writeError(w, r, errConflict("不能降级或停用自己的管理员账号"))
			return
		}
	}

	out, err := s.deps.Users.UpdateProfile(r.Context(), id, role, status, quota)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminTopup 手工充值/扣减。
//
// idempotency_key 必填而不是可选：充值是唯一一个"重复执行会凭空造出积分"的
// 写操作，管理员双击一次就多充一份。同 key 二次调用由 Ledger 回 409。
func (s *server) handleAdminTopup(w http.ResponseWriter, r *http.Request) {
	userID, err := pathID(r, "userId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Amount         int    `json:"amount"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	var fields []domain.FieldError
	if req.Amount == 0 {
		fields = append(fields, domain.FieldError{Key: "amount", Message: "不能为 0（正数充值，负数扣减）"})
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		fields = append(fields, domain.FieldError{Key: "idempotency_key", Message: "必填"})
	}
	if len(fields) > 0 {
		writeError(w, r, errFields(fields, "参数校验未通过"))
		return
	}

	operator, _ := IdentityFrom(r.Context())
	entry, err := s.deps.Ledger.Topup(r.Context(), userID, req.Amount,
		strings.TrimSpace(req.Reason), operator.UserID, strings.TrimSpace(req.IdempotencyKey))
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 被充值的那个人此刻很可能正开着页面等余额变化。
	s.publishBalance(r.Context(), userID)
	writeJSON(w, http.StatusOK, entry)
}

func (s *server) handleAdminUserLedger(w http.ResponseWriter, r *http.Request) {
	userID, err := pathID(r, "userId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursor, limit := pagination(r)
	page, err := s.deps.Ledgers.List(r.Context(), userID, cursor, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pageOf(page.Items, page.NextCursor))
}

// ── 任务 ────────────────────────────────────────────────────────

// handleAdminListTasks 是跨用户的任务查询，排障入口。
//
// 与用户侧 GET /api/tasks 的唯一区别是不强制按 user_id 过滤——
// 同一个 TaskFilter、同一个仓储方法，管理端只是少加一个条件。
//
// IncludeDismissed 恒为 true：用户把一张失败卡片收起来，不该让那条任务
// 从故障排查视野里消失。
func (s *server) handleAdminListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor, limit := pagination(r)
	f := domain.TaskFilter{
		UserID:           q.Get("user_id"),
		ModelID:          q.Get("model_id"),
		Cursor:           cursor,
		Limit:            limit,
		IncludeDismissed: true,
	}
	if v := q.Get("status"); v != "" {
		for _, raw := range strings.Split(v, ",") {
			switch st := domain.TaskStatus(strings.TrimSpace(raw)); st {
			case domain.TaskStatusQueued, domain.TaskStatusRunning, domain.TaskStatusSucceeded,
				domain.TaskStatusFailed, domain.TaskStatusCanceled:
				f.Statuses = append(f.Statuses, st)
			default:
				writeError(w, r, errInvalid("status 取值非法: %s", raw))
				return
			}
		}
	}
	if v := q.Get("error_code"); v != "" {
		f.ErrorCode = domain.TaskErrorCode(v)
	}

	page, err := s.deps.Tasks.List(r.Context(), f)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pageOf(page.Items, page.NextCursor))
}

// statsWindows 是允许的统计窗口。写死成枚举而不是收任意 duration：
// 任意窗口会让人顺手查 90d，而那是一次全表扫描。
var statsWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

func (s *server) handleAdminTaskStats(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("window")
	if label == "" {
		label = "24h"
	}
	d, ok := statsWindows[label]
	if !ok {
		writeError(w, r, errInvalid("window 取值非法: %s（可选 1h / 24h / 7d）", label))
		return
	}
	stats, err := s.deps.Tasks.Stats(r.Context(), d, label)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleAdminRetryTask 把一条终态任务打回队列。
//
// **不重新冻结积分**：原来那笔 hold 从未退还（失败退款走的是 Refund，
// 而能重试的场景恰恰是那些没退款的），重新冻结会双倍扣。
//
// **只重试 executor 执行的任务**：画布 chat / storyboard / refine 三条链路
// 也在 tasks 里落行（为了让积分流水有一个可对账的 task_id，见 chatbill.go），
// 但它们是在 HTTP handler 里同步跑完的，产物落点是 canvas_cards。把这种行
// 打回 queued，worker 会照着 prompt 真打一次上游、真花一份钱，而产物只能
// 转存成一个孤儿 text 资产——用户画布上那张卡纹丝不动。重试在这里不是
// "再试一次"，是"再花一次钱换一个没人看得到的产物"，所以直接拒绝。
func (s *server) handleAdminRetryTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "taskId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.deps.Tasks.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if t.Step != "" {
		writeError(w, r, errConflict("画布对话类任务（%s）不支持重试，请在画布上重新发起", t.Step))
		return
	}
	if !t.Status.IsTerminal() {
		writeError(w, r, errConflict("任务当前是 %s，只有终态任务能重试", t.Status))
		return
	}
	if err := s.deps.Tasks.Requeue(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	next, err := s.deps.Tasks.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.publish(r.Context(), stream.TaskUpdated(next.UserID, next))
	writeJSON(w, http.StatusOK, next)
}

func (s *server) handleAdminCancelTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "taskId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.deps.Tasks.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	next, err := s.cancelTask(r.Context(), t)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

// ── 运维 ────────────────────────────────────────────────────────

func (s *server) handleAdminBreakers(w http.ResponseWriter, r *http.Request) {
	if s.deps.Breakers == nil {
		writeJSON(w, http.StatusOK, map[string]any{"breakers": []domain.BreakerSnapshot{}})
		return
	}
	items, err := s.deps.Breakers.Snapshot(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if items == nil {
		items = []domain.BreakerSnapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"breakers": items})
}

func (s *server) handleAdminStorageUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.deps.Assets.StorageUsage(r.Context(), queryInt(r, "top", 10))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if usage.TopUsers == nil {
		usage.TopUsers = []domain.StorageUserUsage{}
	}
	writeJSON(w, http.StatusOK, usage)
}

// cleanupSampleLimit 是回显给管理员的候选样本上限。
// 够判断"选中的是不是我想删的那批"，又不至于把响应撑成一份文件清单。
const cleanupSampleLimit = 20

// cleanupBatchLimit 是单次清理扫描的条数上限。
// 分批而不是一次清完：一次请求删几万个文件会把 HTTP 超时耗尽，
// 而清理天然可以多按几次。
const cleanupBatchLimit = 500

// handleAdminStorageCleanup 回收可清理的字节。
//
// **dry_run 默认 true。** 删文件不可逆，默认真删意味着一次手滑就是数据丢失；
// 默认预演的代价只是管理员要多按一次按钮。
func (s *server) handleAdminStorageCleanup(w http.ResponseWriter, r *http.Request) {
	req := struct {
		DryRun        *bool                `json:"dry_run"`
		OlderThanDays *int                 `json:"older_than_days"`
		Target        domain.CleanupTarget `json:"target"`
	}{}
	// 空 body 合法：全走默认值（预演、30 天、过期上传）。
	_ = decodeJSON(w, r, &req)

	dryRun := req.DryRun == nil || *req.DryRun
	days := 30
	if req.OlderThanDays != nil {
		if *req.OlderThanDays < 0 {
			writeError(w, r, errFields([]domain.FieldError{
				{Key: "older_than_days", Message: "不能为负"}}, "参数校验未通过"))
			return
		}
		days = *req.OlderThanDays
	}
	target := req.Target
	if target == "" {
		target = domain.CleanupExpiredUploads
	}
	before := s.now().Add(-time.Duration(days) * 24 * time.Hour)

	var (
		res domain.CleanupResult
		err error
	)
	switch target {
	case domain.CleanupExpiredUploads:
		res, err = s.cleanupExpiredUploads(r.Context(), before, dryRun)
	case domain.CleanupSoftDeletedAssets:
		res, err = s.cleanupSoftDeletedAssets(r.Context(), before, dryRun)
	case domain.CleanupOrphanFiles:
		// 孤儿文件要反向扫存储后端并与 assets 表比对，而 asset.Store
		// 没有 List——加一个只为清理服务的枚举接口，会逼着未来每个
		// 对象存储实现都提供一份可能有几十万条的全量列举。留给离线脚本。
		writeError(w, r, errInvalid("orphan_files 清理需离线执行，暂不支持在线触发"))
		return
	default:
		writeError(w, r, errInvalid("target 取值非法: %s", target))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	res.DryRun = dryRun
	writeJSON(w, http.StatusOK, res)
}

func (s *server) cleanupExpiredUploads(ctx context.Context, before time.Time, dryRun bool) (domain.CleanupResult, error) {
	items, err := s.deps.Uploads.ListExpired(ctx, before, cleanupBatchLimit)
	if err != nil {
		return domain.CleanupResult{}, err
	}
	res := domain.CleanupResult{CandidateCount: len(items)}
	for _, u := range items {
		res.ReclaimedBytes += u.Bytes
		if len(res.Samples) < cleanupSampleLimit {
			res.Samples = append(res.Samples, u.ID)
		}
		if dryRun {
			continue
		}
		// 先删字节再删记录：反过来的话记录没了、字节还在，就成了永远找不到
		// 主人的孤儿文件。这个顺序下最坏情况是记录还在但字节没了，
		// 而 upload 本来就允许过期取不到。
		if err := s.deps.Blobs.Delete(ctx, u.StorageKey); err != nil {
			return res, err
		}
		if err := s.deps.Uploads.Delete(ctx, u.ID); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *server) cleanupSoftDeletedAssets(ctx context.Context, before time.Time, dryRun bool) (domain.CleanupResult, error) {
	items, err := s.deps.Assets.ListSoftDeleted(ctx, before, cleanupBatchLimit)
	if err != nil {
		return domain.CleanupResult{}, err
	}
	res := domain.CleanupResult{CandidateCount: len(items)}
	for _, a := range items {
		res.ReclaimedBytes += a.Bytes
		if len(res.Samples) < cleanupSampleLimit {
			res.Samples = append(res.Samples, a.ID)
		}
		if dryRun {
			continue
		}
		for _, key := range []string{a.StorageKey, derefStr(a.ThumbKey), derefStr(a.PosterKey)} {
			if key == "" {
				continue
			}
			if err := s.deps.Blobs.Delete(ctx, key); err != nil {
				return res, err
			}
		}
		if err := s.deps.Assets.HardDelete(ctx, a.ID); err != nil {
			return res, err
		}
		// 用量缓存是累加出来的，硬删不会自动回退。就地重算比记一笔负数
		// 增量可靠：崩在中间也只是下次重算修正。
		if s.deps.Quota != nil {
			if _, err := s.deps.Quota.Recompute(ctx, a.UserID); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}
