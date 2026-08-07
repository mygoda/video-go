package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// createTaskRequest 逐字段镜像 docs/contracts/capability-schema.ts 的同名 interface。
//
// **没有 mode 字段。** 「传了参考图走图生图 / 没传走文生」是隐式分流：
// 服务端按 inputs 是否为空路由，前端只发 prompt + inputs。多一个 mode
// 就等于要求前端知道每个模型有几种模式，那正是 capability schema 想消灭的东西。
type createTaskRequest struct {
	ModelID     string              `json:"model_id"`
	Prompt      string              `json:"prompt"`
	Inputs      map[string][]string `json:"inputs"`
	Params      map[string]any      `json:"params"`
	ClientToken string              `json:"client_token"`
	CanvasID    string              `json:"canvas_id"`
	CardID      string              `json:"card_id"`
}

type createTaskResponse struct {
	TaskID        string            `json:"task_id"`
	Status        domain.TaskStatus `json:"status"`
	EstimatedCost int               `json:"estimated_cost"`
	QueuePosition *int              `json:"queue_position"`
	ETASeconds    *float64          `json:"eta_seconds"`
}

func taskAccepted(t domain.Task) createTaskResponse {
	return createTaskResponse{
		TaskID:        t.ID,
		Status:        t.Status,
		EstimatedCost: t.EstimatedCost,
		QueuePosition: t.QueuePosition,
		ETASeconds:    t.ETASeconds,
	}
}

// handleCreateTask 是整条链路的入口：校验 → 计价 → 落库 → 冻结 → 入队。
//
// # 为什么必须先落库再冻结
//
// credit_ledger.task_id 上有指向 tasks 的外键，冻结流水写不进一条还不存在的
// 任务。因此顺序只能是 Create → Hold，而 Create 出来的行状态就是 queued，
// worker 的 Claim 立刻就能捞走。
//
// 这留下一个窄窗口：worker 在 Hold 返回前就开始执行。窗口里 Hold 仍会照常
// 发生（差的是毫秒级），唯一真正的坏情况是 Hold 失败——所以余额检查提前到
// 落库之前做，让 Hold 在正常路径上不可能失败；真撞上并发耗尽余额时，
// 下面的补偿会把任务打回 failed/insufficient_credit 且不扣费。
//
// 反过来做（先冻结再落库）要绕开外键，代价是流水里出现一批指不到任务的孤儿
// hold 记录，对账时分不清"这笔冻结对应哪次生成"——那比一个窄窗口贵得多。
func (s *server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req createTaskRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	if strings.TrimSpace(req.ModelID) == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "model_id", Message: "必填"}}, "参数校验未通过"))
		return
	}
	if req.ClientToken == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "client_token", Message: "必填，用于幂等提交"}}, "参数校验未通过"))
		return
	}

	// 幂等：断网重发拿回同一个任务，而不是又生成一次、又冻结一次。
	if existing, ok, err := s.deps.Tasks.GetByClientToken(ctx, id.UserID, req.ClientToken); err != nil {
		writeError(w, r, err)
		return
	} else if ok {
		writeJSON(w, http.StatusOK, taskAccepted(existing))
		return
	}

	model, err := s.deps.Models.Get(ctx, req.ModelID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !model.Enabled {
		writeError(w, r, errInvalid("模型 %s 当前不可用", req.ModelID))
		return
	}
	schema, err := capability.DecodeSchema(model.Capability)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}

	sub := capability.Submission{
		ModelID: req.ModelID,
		Prompt:  req.Prompt,
		Inputs:  req.Inputs,
		Params:  req.Params,
	}
	if fields := s.deps.Validator.Validate(schema, sub); len(fields) > 0 {
		writeError(w, r, errFields(fields, "参数校验未通过"))
		return
	}
	// 输入槽引用的 upload 必须是本人的。校验器只管"槽填得对不对"，
	// 管不到"这个 upload_id 是谁的"——少了这一步，猜到一个 id 就能拿别人的图去生成。
	if err := s.assertOwnsUploads(ctx, id.UserID, req.Inputs); err != nil {
		writeError(w, r, err)
		return
	}

	if max := schema.Limits.MaxConcurrentPerUser; max > 0 {
		running, err := s.deps.Tasks.CountRunningByModel(ctx, id.UserID, req.ModelID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if running >= max {
			writeError(w, r, &domain.Error{
				Code:      domain.CodeRateLimited,
				Message:   "该模型的并发上限是 " + itoa(max) + " 个任务",
				Retryable: true,
			})
			return
		}
	}

	cost, err := s.deps.Pricer.Estimate(schema.Pricing, capability.EvalContext{
		Params: req.Params,
		Inputs: req.Inputs,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// 落库前先看一眼余额，见本函数的头注释。
	if bal, err := s.deps.Ledger.Balance(ctx, id.UserID); err == nil && bal.Available < cost {
		writeError(w, r, &domain.Error{
			Code:    domain.CodeInsufficientCredit,
			Message: "积分不足：需要 " + itoa(cost) + "，可用 " + itoa(bal.Available),
		})
		return
	}

	eta := schema.ETA.P50Seconds
	task, err := s.deps.Tasks.Create(ctx, domain.Task{
		ID:            uid.New(),
		UserID:        id.UserID,
		ModelID:       model.ID,
		ProviderID:    model.ProviderID,
		Status:        domain.TaskStatusQueued,
		Prompt:        req.Prompt,
		Params:        req.Params,
		Inputs:        req.Inputs,
		EstimatedCost: cost,
		CanvasID:      strPtrOrNil(req.CanvasID),
		CardID:        strPtrOrNil(req.CardID),
		ClientToken:   req.ClientToken,
		CreatedAt:     s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	if _, err := s.deps.Ledger.Hold(ctx, id.UserID, task.ID, cost); err != nil {
		// 冻结失败的补偿：把任务判失败并明确标注未扣费。
		// expect=queued 保证 worker 已经捞走时不会覆盖它的状态。
		terr := taskErrorFrom(err)
		_ = s.deps.Tasks.UpdateStatus(detachedContext(), task.ID,
			domain.TaskStatusQueued, domain.TaskStatusFailed, &terr)
		writeError(w, r, err)
		return
	}

	if eta > 0 {
		task.ETASeconds = &eta
	}
	s.publish(ctx, stream.TaskUpdated(id.UserID, task))
	s.publishBalance(ctx, id.UserID)

	writeJSON(w, http.StatusCreated, taskAccepted(task))
}

// assertOwnsUploads 校验 inputs 里引用的每个 upload 都属于调用方。
func (s *server) assertOwnsUploads(ctx context.Context, userID string, inputs map[string][]string) error {
	for slot, ids := range inputs {
		for _, uploadID := range ids {
			if uploadID == "" {
				continue
			}
			u, err := s.deps.Uploads.Get(ctx, uploadID)
			if err != nil {
				var de *domain.Error
				if errors.As(err, &de) && de.Code == domain.CodeNotFound {
					return errFields([]domain.FieldError{
						{Key: slot, Message: "上传对象 " + uploadID + " 不存在或已过期"},
					}, "参数校验未通过")
				}
				return err
			}
			if u.UserID != userID {
				return errFields([]domain.FieldError{
					{Key: slot, Message: "上传对象 " + uploadID + " 不存在或已过期"},
				}, "参数校验未通过")
			}
		}
	}
	return nil
}

func (s *server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()
	status := r.URL.Query().Get("status")

	// active 是 SSE 重连后的对账接口：必须返回全部未终结任务，
	// 分页会让"我漏了哪些事件"这个问题重新变得无解。
	if status == "active" {
		items, err := s.deps.Tasks.ListActive(ctx, id.UserID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		s.attachAssets(ctx, items)
		writeJSON(w, http.StatusOK, pageOf(items, ""))
		return
	}

	cursor, limit := pagination(r)
	f := domain.TaskFilter{
		UserID:   id.UserID,
		CanvasID: r.URL.Query().Get("canvas_id"),
		Cursor:   cursor,
		Limit:    limit,
	}
	switch status {
	case "", "all":
	case string(domain.TaskStatusQueued), string(domain.TaskStatusRunning),
		string(domain.TaskStatusSucceeded), string(domain.TaskStatusFailed),
		string(domain.TaskStatusCanceled):
		f.Statuses = []domain.TaskStatus{domain.TaskStatus(status)}
	default:
		writeError(w, r, errInvalid("status 取值非法: %s", status))
		return
	}

	page, err := s.deps.Tasks.List(ctx, f)
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.attachAssets(ctx, page.Items)
	writeJSON(w, http.StatusOK, pageOf(page.Items, page.NextCursor))
}

func (s *server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	taskID, err := pathID(r, "taskId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.deps.Tasks.Get(r.Context(), taskID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := requireOwner(id, t.UserID, "任务"); err != nil {
		writeError(w, r, err)
		return
	}
	one := []domain.Task{t}
	s.attachAssets(r.Context(), one)
	writeJSON(w, http.StatusOK, one[0])
}

func (s *server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	taskID, err := pathID(r, "taskId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	t, err := s.deps.Tasks.Get(r.Context(), taskID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := requireOwner(id, t.UserID, "任务"); err != nil {
		writeError(w, r, err)
		return
	}
	t, err = s.cancelTask(r.Context(), t)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// cancelTask 取消一条任务并退回冻结积分。用户端与管理端共用。
//
// 取消**一律不扣费**——queued 根本没开始，running 也没拿到产物，
// 「只要没拿到产物就不收钱」这条规则不因取消发起方是谁而变。
func (s *server) cancelTask(ctx context.Context, t domain.Task) (domain.Task, error) {
	if t.Status.IsTerminal() {
		return domain.Task{}, errConflict("任务已处于终态 %s，无法取消", t.Status)
	}

	// 尽力通知上游。上游不支持取消（或调用失败）不影响本地判定：
	// 用户点了取消，本地就该停止为它花钱，上游那份算沉没成本。
	if s.deps.Runner != nil {
		if err := s.deps.Runner.Cancel(ctx, t.ID); err != nil {
			s.logCancelUpstream(t.ID, err)
		}
	}

	if err := s.deps.Tasks.UpdateStatus(ctx, t.ID, t.Status, domain.TaskStatusCanceled, nil); err != nil {
		return domain.Task{}, err
	}
	if _, err := s.deps.Ledger.Refund(ctx, t.UserID, t.ID, "task canceled"); err != nil {
		// 退款失败必须留痕：钱还冻着，得有人能查出来。但任务确实已取消了，
		// 回一个 500 会让前端以为取消没成功而重试，反而更糟。
		s.logRefund(t.ID, err)
	}

	next, err := s.deps.Tasks.Get(ctx, t.ID)
	if err != nil {
		return domain.Task{}, err
	}
	s.publish(ctx, stream.TaskUpdated(t.UserID, next))
	s.publishBalance(ctx, t.UserID)
	return next, nil
}

// attachAssets 给终态成功的任务补上产物列表。
//
// 列表接口本可以少查这一轮，但前端的瀑布流要直接显示产物缩略图；
// 不带回去它就得为每条任务再发一个请求，一屏 20 张图就是 20 个往返。
func (s *server) attachAssets(ctx context.Context, tasks []domain.Task) {
	for i := range tasks {
		if tasks[i].Status != domain.TaskStatusSucceeded {
			continue
		}
		assets, err := s.deps.Assets.ListByTask(ctx, tasks[i].ID)
		if err != nil {
			continue
		}
		tasks[i].Assets = s.decorateAssets(assets)
	}
}
