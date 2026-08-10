package httpapi

import (
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

// handleCanvasChat 记一条对话消息，把它当作创作起点写成一份剧本，
// 落成画布上的一张 script 卡。
//
// 用户消息先落库再调模型：那句话是用户真的说过的，上游挂了也不该丢。
// 写剧本本身是同步的（chat 族一次往返就有结果），因此响应里直接带回新增的
// 那张卡片——见 chatcall.go 的包内注释。
//
// # 这一步到此为止：不拆镜头，不派任何出片任务
//
// 上一版在这里一次调用就把剧本和分镜两层一起吞掉，直接落 3 张视频卡并为
// 每张卡起一条真的视频任务：**用户还没看到任何东西，钱已经花出去了**，
// 而中间的剧本、分镜从来没有作为对象存在过，看不见也就改不动。
// 现在这一步只产出一张剧本卡，`tasks` 表一行不增。拆分镜是下一步，
// 出片还要更后面；每一层都得先停下来让用户看一眼、改一改。
//
// 因此这里也不再解析技能的 default_model_id：这一步不出片，就没有"出片模型
// 配错了"这回事。它挪到真正花钱的那一步去校验，才校验得到点子上。
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

	draft, trace, err := s.generateScript(ctx, req.Message, pickCards(snap.Cards, req.RefCardIDs))
	if err != nil {
		writeError(w, r, err)
		return
	}
	logChatCall("script", p.ID, trace)

	card := scriptCard(draft, snap.Cards, req.RefCardIDs, s.now())
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision,
		[]domain.CanvasOp{{Type: domain.OpCardCreate, Card: &card}})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// 助手消息的 RefCardIDs 指向这轮产出的卡片：对话记录因此能回答
	// "这张卡是哪句话变出来的"，而这正是「创作过程可回放」的一半。
	reply, err := s.deps.Canvases.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleAssistant,
		Content:    scriptReply(draft, trace.ModelID),
		RefCardIDs: []string{card.ID},
		CreatedAt:  s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// task_ids 恒定回一个空数组而不是省掉这个字段：前端读它的 .length，
	// 而"这一步不派任务"要作为一个明确的事实回过去，不是一次字段缺失。
	writeJSON(w, http.StatusAccepted, map[string]any{
		"message_id":       m.ID,
		"reply_message_id": reply.ID,
		"revision":         revision,
		"task_ids":         []string{},
		"cards":            []domain.Card{card},
	})
}

// handleCanvasStoryboard 把一张剧本卡拆成 N 张镜头卡。
//
// 创作链路的第二步，由用户点着某张剧本卡主动发起——**剧本那一步不会自动
// 往下走**。理由同 handleCanvasChat：每一层都得先停下来让用户看一眼、改一改，
// 而链路一旦自动跑到底，中间那几层就又只是过程，不是能改的对象了。
//
// # 这一步只落文字，一分钱不花
//
// 拆镜头本身走 chat 模型（AIGC_STORYBOARD_MODEL），成本可忽略；除了那一次
// chat 调用，**不派任何出片任务，`tasks` 表一行不增**。首帧图、成片都是后面
// 的事。task_ids 因此恒定回空数组，理由同 handleCanvasChat。
//
// # 镜头数是入参
//
// 不传取 storyboardDefaultShots，上限 storyboardMaxShots，越界当场报错。
// 写死一个用户看不见的常量当成本闸门，代价是想要 5 个镜头的人在界面上
// 找不到任何地方能说这句话——见 storyboard.go 里那两个常量的注释。
func (s *server) handleCanvasStoryboard(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		CardID    string `json:"card_id"`
		ShotCount *int   `json:"shot_count"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.CardID) == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "card_id", Message: "必填，要拆的那张剧本卡"}}, "参数校验未通过"))
		return
	}
	shotCount, err := resolveShotCount(req.ShotCount)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := r.Context()

	snap, err := s.deps.Canvas.Snapshot(ctx, p.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	src, err := storyboardSource(snap.Cards, strings.TrimSpace(req.CardID))
	if err != nil {
		writeError(w, r, err)
		return
	}

	// 上下文取剧本卡自己的血缘上游：那几张卡（用户当初勾选的参考）正是这份
	// 剧本的风格来源，分镜跟着它们走才不会换一套调子。
	shots, trace, err := s.generateStoryboard(ctx, derefStr(src.Text), shotCount, pickCards(snap.Cards, src.Refs))
	if err != nil {
		writeError(w, r, err)
		return
	}
	logChatCall("storyboard", p.ID, trace)

	cards := shotCards(shots, src.ID, snap.Cards, s.now())
	ops := make([]domain.CanvasOp, 0, len(cards))
	cardIDs := make([]string, 0, len(cards))
	for i := range cards {
		ops = append(ops, domain.CanvasOp{Type: domain.OpCardCreate, Card: &cards[i]})
		cardIDs = append(cardIDs, cards[i].ID)
	}
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision, ops)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// 只落一条助手消息，不伪造一条用户消息：这一步是点卡片触发的，用户
	// 没说过任何话，替他记一句会让对话记录变成假的。
	reply, err := s.deps.Canvases.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleAssistant,
		Content:    storyboardReply(src.Title, len(cards), trace.ModelID),
		RefCardIDs: cardIDs,
		CreatedAt:  s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"reply_message_id": reply.ID,
		"revision":         revision,
		"task_ids":         []string{},
		"cards":            cards,
	})
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
