package httpapi

import (
	"fmt"
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

// handleCanvasChat 记一条对话消息，并把它当作脚本拆成分镜卡片。
//
// 用户消息先落库再调模型：脚本是用户真的说过的话，上游挂了也不该丢。
// 拆解本身是同步的（chat 族一次往返就有结果），因此响应里直接带回新增卡片，
// 而不是回一串要前端再去查的 task_id——见 storyboard.go 的包内注释。
//
// task_ids 仍是空数组，但含义变了：这条链路**确实没有产生生成任务**，
// 分镜的产物是文字。等到"分镜卡片一键转图"接上时，那里才会有真任务。
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

	cards := storyboardCards(shots, snap.Cards, req.RefCardIDs, s.now())
	ops := make([]domain.CanvasOp, 0, len(cards))
	for i := range cards {
		ops = append(ops, domain.CanvasOp{Type: domain.OpCardCreate, Card: &cards[i]})
	}
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision, ops)
	if err != nil {
		writeError(w, r, err)
		return
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
		Content:    storyboardReply(len(cards), trace.ModelID),
		RefCardIDs: ids,
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
		"task_ids":         []string{},
		"cards":            cards,
	})
}

// storyboardReply 是落进对话里的那条助手消息。
func storyboardReply(n int, modelID string) string {
	return fmt.Sprintf("已用 %s 拆出 %d 个镜头，并在画布上创建了对应的文本卡片。", modelID, n)
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
