package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

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

	task, created, err := s.submitTask(r.Context(), id.UserID, req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, taskAccepted(task))
		return
	}
	writeJSON(w, http.StatusCreated, taskAccepted(task))
}

// submitTask 是「提交一条生成任务」的完整流程：幂等命中 → 校验 → 计价 →
// 落库 → 冻结 → 推事件。created 为 false 表示这是一次幂等命中，任务早就存在。
//
// 抽出来是因为它有第二个调用方：画布的分镜拆解要为每个镜头起一条真任务
// （见 handlers_canvas.go）。那条路要的与 REST 入口**一字不差**——同一套
// 参数校验、同一套并发上限、同一套先查余额再落库再冻结的顺序。复制一份的话，
// 两条路迟早在某次修改后对同一份提交给出不同的判定，而那种分叉在账上体现
// 出来的时候已经晚了。
func (s *server) submitTask(ctx context.Context, userID string, req createTaskRequest) (domain.Task, bool, error) {
	// 幂等：断网重发拿回同一个任务，而不是又生成一次、又冻结一次。
	if existing, ok, err := s.deps.Tasks.GetByClientToken(ctx, userID, req.ClientToken); err != nil {
		return domain.Task{}, false, err
	} else if ok {
		return existing, false, nil
	}

	model, err := s.deps.Models.Get(ctx, req.ModelID)
	if err != nil {
		return domain.Task{}, false, err
	}
	if !model.Enabled {
		return domain.Task{}, false, errInvalid("模型 %s 当前不可用", req.ModelID)
	}
	schema, err := capability.DecodeSchema(model.Capability)
	if err != nil {
		return domain.Task{}, false, errInternal(err)
	}

	sub := capability.Submission{
		ModelID: req.ModelID,
		Prompt:  req.Prompt,
		Inputs:  req.Inputs,
		Params:  req.Params,
	}
	if fields := s.deps.Validator.Validate(schema, sub); len(fields) > 0 {
		return domain.Task{}, false, errFields(fields, "参数校验未通过")
	}
	// 输入槽引用的素材（upload_id 或已有 asset_id）必须是本人的，且要满足槽声明的
	// 文件约束。校验器只管"槽填得对不对"，管不到"这个 id 是谁的、那张图多大"——
	// 少了归属校验，猜到一个 id 就能拿别人的图去生成；少了尺寸校验，一张 200×200
	// 的首帧要等到几十秒后上游 failed 才知道，而钱已经冻上了。
	if err := s.assertInputRefs(ctx, userID, schema, req.Inputs); err != nil {
		return domain.Task{}, false, err
	}

	if max := schema.Limits.MaxConcurrentPerUser; max > 0 {
		running, err := s.deps.Tasks.CountRunningByModel(ctx, userID, req.ModelID)
		if err != nil {
			return domain.Task{}, false, err
		}
		if running >= max {
			return domain.Task{}, false, &domain.Error{
				Code:      domain.CodeRateLimited,
				Message:   "该模型的并发上限是 " + itoa(max) + " 个任务",
				Retryable: true,
			}
		}
	}

	cost, err := s.deps.Pricer.Estimate(schema.Pricing, capability.EvalContext{
		Params: req.Params,
		Inputs: req.Inputs,
	})
	if err != nil {
		return domain.Task{}, false, err
	}

	// 落库前先看一眼余额，见 handleCreateTask 的头注释。
	if bal, err := s.deps.Ledger.Balance(ctx, userID); err == nil && bal.Available < cost {
		return domain.Task{}, false, &domain.Error{
			Code:    domain.CodeInsufficientCredit,
			Message: "积分不足：需要 " + itoa(cost) + "，可用 " + itoa(bal.Available),
		}
	}

	eta := schema.ETA.P50Seconds
	task, err := s.deps.Tasks.Create(ctx, domain.Task{
		ID:            uid.New(),
		UserID:        userID,
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
		return domain.Task{}, false, err
	}

	if _, err := s.deps.Ledger.Hold(ctx, userID, task.ID, cost); err != nil {
		// 冻结失败的补偿：把任务判失败并明确标注未扣费。
		// expect=queued 保证 worker 已经捞走时不会覆盖它的状态。
		terr := taskErrorFrom(err)
		_ = s.deps.Tasks.UpdateStatus(detachedContext(), task.ID,
			domain.TaskStatusQueued, domain.TaskStatusFailed, &terr)
		return domain.Task{}, false, err
	}

	if eta > 0 {
		task.ETASeconds = &eta
	}
	s.publish(ctx, stream.TaskUpdated(userID, task))
	s.publishBalance(ctx, userID)

	return task, true, nil
}

// assertInputRefs 校验 inputs 里引用的每个 id 都属于调用方，
// 并按 InputSlotSpec 核对文件本身的约束（MIME / 字节数 / 像素）。
//
// **一个 id 既可以是 upload_id，也可以是已有的 asset_id**——先按 asset 查，
// 查不到再按 upload 查，与 executor.resolveOne 的顺序一致。这条回落是必须的：
// 少了它，「拿画布上已有的图当首帧」只能把资产的字节取回来重传一遍，同一份
// 图在存储里躺好几份、各计一次配额，任务的血缘边还会指到一个用户从没见过的副本 id。
//
// 文件约束为什么落在这里而不是 capability.Validator：那一层只拿得到 id，
// 判文件属性就得回查存储，校验器也就不再是纯函数（见它的头注释）。而这里本来
// 就要为归属校验把每个素材取一遍，元信息顺手就在手上，多这几个判断不多一次 IO。
// **两条来源分支共用同一套判定**，否则换个来源就能绕过尺寸下限。
//
// 为什么不放在上传那一步：上传时还不知道这份素材要挂到哪个模型的哪个槽上，
// 而约束是**按槽**声明的——同一张 200×200 的图对参考图槽合法、对首帧槽不合法。
// asset 更是连"上传那一步"都没有，它是平台自己生成的产物。
func (s *server) assertInputRefs(ctx context.Context, userID string, schema capability.ModelCapabilitySchema, inputs map[string][]string) error {
	specs := make(map[string]capability.InputSlotSpec, len(schema.Inputs))
	for _, spec := range schema.Inputs {
		specs[spec.Key] = spec
	}

	var fields []domain.FieldError
	for slot, ids := range inputs {
		for _, refID := range ids {
			if refID == "" {
				continue
			}
			ref, err := s.resolveInputRef(ctx, userID, refID)
			if err != nil {
				if isNotFound(err) {
					return errFields([]domain.FieldError{
						{Key: slot, Message: inputRefNotFound(refID)},
					}, "参数校验未通过")
				}
				return err
			}
			spec, ok := specs[slot]
			if !ok {
				// 未知槽已由 Validator 报过，这里不重复报。
				continue
			}
			if msg := checkInputAgainstSlot(spec, ref); msg != "" {
				fields = append(fields, domain.FieldError{Key: slot, Message: msg})
			}
		}
	}
	if len(fields) > 0 {
		return errFields(fields, "参数校验未通过")
	}
	return nil
}

// inputRef 是输入槽里一个 id 解析出来的素材，屏蔽它来自 uploads 还是 assets。
// 槽约束只关心文件属性，两种来源在这一层不该有区别。
type inputRef struct {
	mime   string
	bytes  int64
	width  *int
	height *int
}

// inputRefNotFound 是"这个 id 用不了"的**唯一**措辞。
//
// 不区分「不存在」「已过期」「是别人的」是有意的：区分开就等于给了一个
// 探测接口——拿 id 挨个试，能从报错里读出哪些 id 在库里真实存在、属于谁。
func inputRefNotFound(refID string) string {
	return "输入素材 " + refID + " 不存在或已过期（既不是你的上传对象，也不是你的资产）"
}

// resolveInputRef 把一个输入 id 解析成素材元信息：先 asset 后 upload。
//
// 顺序与 executor.resolveOne 对齐——执行器先查 asset，提交校验也必须先查 asset，
// 否则同一个 id 在两处解析成不同的东西。
//
// **不属于调用方的资源一律走"查不到"这条路，不提前返回。** 直接返回一个
// 「是别人的」错误哪怕文案相同，也容易在后续改动里分叉；让它继续往下找，
// 最终落到与"根本不存在"完全同一个 return 上，存在性就没有泄露的缝隙。
func (s *server) resolveInputRef(ctx context.Context, userID, refID string) (inputRef, error) {
	if s.deps.Assets != nil {
		a, err := s.deps.Assets.Get(ctx, refID)
		switch {
		case err == nil && a.UserID == userID:
			return inputRef{mime: a.MIME, bytes: a.Bytes, width: a.Width, height: a.Height}, nil
		case err == nil:
			// 别人的资产，当作不存在，继续按 upload 找。
		case !isNotFound(err):
			return inputRef{}, err
		}
	}

	u, err := s.deps.Uploads.Get(ctx, refID)
	if err != nil {
		return inputRef{}, err
	}
	if u.UserID != userID {
		return inputRef{}, &domain.Error{Code: domain.CodeNotFound, Message: inputRefNotFound(refID)}
	}
	return inputRef{mime: u.MIME, bytes: u.Bytes, width: u.Width, height: u.Height}, nil
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

// checkInputAgainstSlot 判定一份素材是否满足槽声明，合法返回空串。
//
// 宽高为 nil 表示解不出尺寸（视频、webp——见 imageDimensions）。此时**放行**：
// 声明了 min_pixels 就拒掉所有解不出尺寸的图，会把 webp 这类合法输入一并误伤。
func checkInputAgainstSlot(spec capability.InputSlotSpec, in inputRef) string {
	if len(spec.Accept) > 0 && !slices.Contains(spec.Accept, in.mime) {
		return "不支持的文件类型 " + in.mime + "，仅支持 " + strings.Join(spec.Accept, " / ")
	}
	if spec.MaxBytes > 0 && in.bytes > spec.MaxBytes {
		return "文件 " + itoa(int(in.bytes)) + " 字节，超过上限 " + itoa(int(spec.MaxBytes)) + " 字节"
	}
	if in.width == nil || in.height == nil {
		return ""
	}
	w, h := *in.width, *in.height
	if p := spec.MinPixels; p != nil && (w < p.Width || h < p.Height) {
		return "尺寸 " + itoa(w) + "×" + itoa(h) + " 小于下限 " + itoa(p.Width) + "×" + itoa(p.Height)
	}
	if p := spec.MaxPixels; p != nil && (w > p.Width || h > p.Height) {
		return "尺寸 " + itoa(w) + "×" + itoa(h) + " 超过上限 " + itoa(p.Width) + "×" + itoa(p.Height)
	}
	return ""
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

// handleDismissTask 兑现 DELETE /api/tasks/{taskId}：把任务从用户自己的
// 任务流里收起来。
//
// **不是真删**。credit_ledger.task_id 指向 tasks，账本是 append-only 的真相；
// 用户按下「移除」想要的也不是销毁审计记录，而是别再让这张卡占着自己的任务流。
// 产物不受影响——删产物是资产库那边的 DELETE /api/assets/{id}。
//
// 只收终态任务。还在跑的任务被收起来之后 SSE 照样推它的进度事件，
// 用户会看到一张刚删掉的卡片又冒出来，所以这里让调用方先取消再移除。
func (s *server) handleDismissTask(w http.ResponseWriter, r *http.Request) {
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
	if !t.Status.IsTerminal() {
		writeError(w, r, errConflict("任务还在进行中（%s），请先取消再移除", t.Status))
		return
	}
	if err := s.deps.Tasks.Dismiss(r.Context(), t.ID, time.Now()); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		// 退款真失败时必须留痕：钱还冻着，得有人能查出来。但任务确实已取消了，
		// 回一个 500 会让前端以为取消没成功而重试，反而更糟。
		// 撞上幂等闸（executor 已经退过）不算失败，logRefund 里分开处理。
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
