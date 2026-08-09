package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func (s *server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursor, limit := pagination(r)
	page, err := s.deps.Canvases.ListProjects(r.Context(), id.UserID, cursor, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pageOf(page.Items, page.NextCursor))
}

func (s *server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "name", Message: "必填"}}, "参数校验未通过"))
		return
	}
	p, err := s.deps.Canvases.CreateProject(r.Context(), domain.Project{
		UserID:    id.UserID,
		Name:      name,
		CreatedAt: s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Name         *string `json:"name"`
		CoverAssetID *string `json:"cover_asset_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			writeError(w, r, errFields(
				[]domain.FieldError{{Key: "name", Message: "不能为空"}}, "参数校验未通过"))
			return
		}
		p.Name = strings.TrimSpace(*req.Name)
	}
	if req.CoverAssetID != nil {
		p.CoverAssetID = strPtrOrNil(*req.CoverAssetID)
	}
	next, err := s.deps.Canvases.UpdateProject(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, next)
}

func (s *server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.deps.Canvases.DeleteProject(r.Context(), p.ID); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleGetCanvas(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	snap, err := s.deps.Canvas.Snapshot(r.Context(), p.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if snap.Cards == nil {
		snap.Cards = []domain.Card{}
	}
	if snap.Conversation == nil {
		snap.Conversation = []domain.Message{}
	}
	writeJSON(w, http.StatusOK, snap)
}

// handlePatchCanvas 落一批增量 op。
//
// 增量而不是全量 PUT：几百张卡片每次几百 KB，拖一下卡片就整份发一遍纯属浪费。
// base_revision 不匹配说明另一个标签页动过同一画布，回 409 让前端拉全量覆盖——
// MVP 明确不做协作，也就不需要 CRDT 那套合并。
func (s *server) handlePatchCanvas(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		BaseRevision int64             `json:"base_revision"`
		Ops          []domain.CanvasOp `json:"ops"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Ops) == 0 {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "ops", Message: "至少要有一个 op"}}, "参数校验未通过"))
		return
	}
	revision, err := s.deps.Canvas.Apply(r.Context(), p.ID, req.BaseRevision, req.Ops)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": revision})
}

// handleCanvasChat 记一条对话消息，把它当作脚本拆成分镜卡片，
// 并为每张卡片起一条真的出片任务。
//
// 用户消息先落库再调模型：脚本是用户真的说过的话，上游挂了也不该丢。
// 拆解本身是同步的（chat 族一次往返就有结果），因此响应里直接带回新增卡片；
// 出片是异步的，响应里的 task_ids 是真任务 id，后续走 SSE——见 storyboard.go
// 的包内注释。
//
// # 为什么是「先建卡、再派任务、再回填 task_id」三步
//
// 派任务要带 card_id（执行层靠它把产物回填到卡片上），所以卡片得先存在——
// 反过来先派任务的话，任务可能在卡片落库之前就成功了，那次回填会打空，
// 产物永远回不到卡片上。而 kind 不在 card.update 的白名单里，建卡时就得
// 定死是 video 还是 text，这也是 storyboardPlan 必须在建卡之前解析的原因。
//
// 上游失败一律回错误码，绝不降级成"消息记下了，画布没动"——那种静默
// 正是这个接口此前最难排查的地方。
func (s *server) handleCanvasChat(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Message     string   `json:"message"`
		SkillID     string   `json:"skill_id"`
		RefCardIDs  []string `json:"ref_card_ids"`
		ClientToken string   `json:"client_token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "message", Message: "必填"}}, "参数校验未通过"))
		return
	}
	ctx := r.Context()

	// 出片模型先解析：技能配错（模型禁用、族别不对）要在花掉一次 chat 调用、
	// 在画布上落下一批卡片之前就报出来。
	plan, err := s.storyboardPlan(ctx, req.SkillID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	m, err := s.deps.Canvases.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleUser,
		Content:    req.Message,
		SkillID:    strPtrOrNil(req.SkillID),
		RefCardIDs: req.RefCardIDs,
		CreatedAt:  s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	snap, err := s.deps.Canvas.Snapshot(ctx, p.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	shots, trace, err := s.storyboard(ctx, req.Message, pickCards(snap.Cards, req.RefCardIDs))
	if err != nil {
		writeError(w, r, err)
		return
	}
	logStoryboard(p.ID, trace)

	cards := storyboardCards(shots, snap.Cards, req.RefCardIDs, plan, s.now())
	ops := make([]domain.CanvasOp, 0, len(cards))
	for i := range cards {
		ops = append(ops, domain.CanvasOp{Type: domain.OpCardCreate, Card: &cards[i]})
	}
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision, ops)
	if err != nil {
		writeError(w, r, err)
		return
	}

	taskIDs, submitErr, err := s.submitShots(ctx, p, cards, plan)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(taskIDs) > 0 {
		revision, err = s.deps.Canvas.Apply(ctx, p.ID, revision, shotTaskOps(cards))
		if err != nil {
			writeError(w, r, err)
			return
		}
	}

	// 助手消息的 RefCardIDs 指向这轮产出的卡片：对话记录因此能回答
	// "这几张卡是哪句话变出来的"，而这正是「创作过程可回放」的一半。
	ids := make([]string, 0, len(cards))
	for _, c := range cards {
		ids = append(ids, c.ID)
	}
	reply, err := s.deps.Canvases.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleAssistant,
		Content:    storyboardReply(len(cards), trace.ModelID, len(taskIDs), submitErr),
		RefCardIDs: ids,
		TaskIDs:    taskIDs,
		CreatedAt:  s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"message_id":       m.ID,
		"reply_message_id": reply.ID,
		"revision":         revision,
		"task_ids":         taskIDs,
		"cards":            cards,
	})
}

// submitShots 为每张镜头卡起一条出片任务，并把 task id 写回 cards[i].TaskID。
//
// 返回三个值：成功派出的 task id、**部分失败**的原因、以及需要中断整个请求的
// 错误。区分后两者是因为它们该有不同的下场：
//
//   - 第 5 个镜头因为积分不够派不出去，前 4 个已经在跑、积分已经冻上了。
//     这时回 402 会让前端以为整件事没发生，而画布上明明多了 5 张卡、
//     账上明明少了 4 段的钱。所以部分失败沿着助手消息说清楚，不掀翻请求。
//   - 一个都没派出去，那就是这次请求彻底没做到用户要的事，如实回错误码。
//
// client_token 取 "shot-" + 卡片 id：卡片 id 每轮新生成，因此它既能挡住
// 同一次请求的重试重复扣费，又不会跨轮撞车。
func (s *server) submitShots(ctx context.Context, p domain.Project, cards []domain.Card, plan storyboardShotPlan) ([]string, error, error) {
	if plan.Model == nil {
		return []string{}, nil, nil
	}

	taskIDs := make([]string, 0, len(cards))
	var firstErr error
	for i := range cards {
		if cards[i].Prompt == nil {
			continue
		}
		t, _, err := s.submitTask(ctx, p.UserID, createTaskRequest{
			ModelID:     plan.Model.ID,
			Prompt:      *cards[i].Prompt,
			Params:      plan.Params,
			ClientToken: "shot-" + cards[i].ID,
			CanvasID:    p.ID,
			CardID:      cards[i].ID,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Warn("分镜卡出片任务提交失败",
				"project_id", p.ID, "card_id", cards[i].ID, "model_id", plan.Model.ID, "err", err)
			continue
		}
		cards[i].TaskID = &t.ID
		taskIDs = append(taskIDs, t.ID)
	}

	if len(taskIDs) == 0 && firstErr != nil {
		return nil, nil, firstErr
	}
	return taskIDs, firstErr, nil
}

// shotTaskOps 把已派出的 task id 回填到卡片上。task_id 在 card.update 的
// 白名单里，卡片类型不在——所以只有这一项需要事后补，见 handleCanvasChat。
func shotTaskOps(cards []domain.Card) []domain.CanvasOp {
	ops := make([]domain.CanvasOp, 0, len(cards))
	for _, c := range cards {
		if c.TaskID == nil {
			continue
		}
		ops = append(ops, domain.CanvasOp{
			Type:  domain.OpCardUpdate,
			ID:    c.ID,
			Patch: map[string]any{"task_id": *c.TaskID},
		})
	}
	return ops
}

// storyboardReply 是落进对话里的那条助手消息。
//
// 派了多少条任务要写进去：用户看到"拆出 3 个镜头"却只有 2 张卡在转圈时，
// 得能在对话里读到第 3 张为什么没动，而不是自己去猜。
func storyboardReply(n int, modelID string, tasks int, submitErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "已用 %s 拆出 %d 个镜头，并在画布上创建了对应的卡片。", modelID, n)
	switch {
	case tasks > 0 && submitErr != nil:
		fmt.Fprintf(&b, "其中 %d 个已开始生成，剩下的没能提交：%s", tasks, asDomainError(submitErr).Message)
	case tasks > 0:
		fmt.Fprintf(&b, "%d 个镜头已开始生成，完成后会自动出现在卡片上。", tasks)
	}
	return b.String()
}

// pickCards 按 id 从快照里挑出卡片，顺序随 ids。
// 找不到的 id 直接跳过：前端本地删过卡片但引用列表没同步是常态，
// 为此回一个 404 只会让用户的一句话发不出去。
func pickCards(cards []domain.Card, ids []string) []domain.Card {
	if len(ids) == 0 {
		return nil
	}
	byID := make(map[string]domain.Card, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}
	out := make([]domain.Card, 0, len(ids))
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

func (s *server) ownedProject(r *http.Request) (domain.Project, error) {
	id, err := identity(r)
	if err != nil {
		return domain.Project{}, err
	}
	projectID, err := pathID(r, "projectId")
	if err != nil {
		return domain.Project{}, err
	}
	p, err := s.deps.Canvases.GetProject(r.Context(), projectID)
	if err != nil {
		return domain.Project{}, err
	}
	if err := requireOwner(id, p.UserID, "画布项目"); err != nil {
		return domain.Project{}, err
	}
	return p, nil
}
