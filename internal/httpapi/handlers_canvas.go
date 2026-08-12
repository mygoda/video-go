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
	if r.URL.Query().Get("trashed") == "1" {
		items, err := s.deps.Canvases.ListTrashedProjects(r.Context(), id.UserID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, pageOf(items, ""))
		return
	}
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

// handleRestoreProject 把回收站里的项目恢复。不走 ownedProject——它只找未删的，
// 恢复目标恰恰是已删的；归属校验放在 store 的 WHERE user_id 里。
func (s *server) handleRestoreProject(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.deps.Canvases.RestoreProject(r.Context(), id.UserID, r.PathValue("projectId")); err != nil {
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
// 现在这一步只产出一张剧本卡，**不派任何出片任务**。拆分镜是下一步，
// 出片还要更后面；每一层都得先停下来让用户看一眼、改一改。
//
// 写剧本这一次 chat 调用本身是要收费的（它烧真 token），因此 tasks 表会多
// 一行——那是这笔积分的结算对象，不是一条会被 worker 捞去执行的任务，
// 理由见 chatbill.go。它不出现在 task_ids 里：那个字段回答的是
// "这一步派了几条出片任务"，答案仍然是零。
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
		// ModelID 是用户这次点名用哪个模型写剧本，可选。
		// 不传就沿用既有规则（AIGC_STORYBOARD_MODEL → 第一个启用的 chat 模型），
		// 行为与本字段存在之前一字不差。
		ModelID string `json:"model_id"`
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

	draft, trace, bill, err := s.generateScript(ctx,
		chatCall{userID: p.UserID, modelID: req.ModelID, projectID: p.ID},
		req.Message, pickCards(snap.Cards, req.RefCardIDs))
	if err != nil {
		writeError(w, r, err)
		return
	}
	logChatCall("script", p.ID, trace)

	card := scriptCard(draft, snap.Cards, req.RefCardIDs, trace.ModelID, s.now())
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision,
		[]domain.CanvasOp{{Type: domain.OpCardCreate, Card: &card}})
	if err != nil {
		// 卡没落上去，用户手里什么都没有：全额退。撞版本冲突也算这一类——
		// 他重试一次会再花一次钱，这一次就不能收。
		bill.refund(ctx, err)
		writeError(w, r, err)
		return
	}
	// 剧本已经在画布上了，这次调用交付完成。往后再失败也不退——
	// 下面那条对话消息没写进去不影响用户拿到的东西。
	bill.charge(ctx)

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
// # 这一步只落文字
//
// 拆镜头本身走 chat 模型（AIGC_STORYBOARD_MODEL），按模型声明的固定单价扣一
// 笔积分；除了那一次 chat 调用，**不派任何出片任务**。首帧图、成片都是后面
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
		CardID        string `json:"card_id"`
		ShotCount     *int   `json:"shot_count"`
		ImageUploadID string `json:"image_upload_id"`
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

	// 传了图就换视觉模型、把图内联进这次拆分镜（见 storyboard.go）。没传图
	// 一切照旧走纯文本模型。图的归属与视觉模型的可用性都在这里先挡一道。
	var img *storyboardImage
	if uploadID := strings.TrimSpace(req.ImageUploadID); uploadID != "" {
		si, err := s.resolveStoryboardImage(ctx, p.UserID, uploadID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		img = si
	}

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
	shots, trace, bill, err := s.generateStoryboard(ctx,
		chatCall{userID: p.UserID, projectID: p.ID, cardID: src.ID},
		derefStr(src.Text), shotCount, pickCards(snap.Cards, src.Refs), img)
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
		bill.refund(ctx, err)
		writeError(w, r, err)
		return
	}
	bill.charge(ctx)

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

// handleRefineScriptCard 按用户的一句指令让模型把一张剧本卡改写一版。
//
// # 为什么改写要走 card.update，而不是直接写这张卡
//
// 剧本卡的版本历史是在**保存正文**这条路径上由服务端自动追加的
// （store/mysql.scriptVersionSet）：读出改动前的标题与正文，
// AppendVersion 收进 params.versions，再把新正文写下去，三步在项目行锁里
// 串行。这里改走一条自己的 SQL 会快一点，代价是用户点了「优化」之后上一版
// 被无声吃掉——而"改坏了能切回去"正是这个按钮敢按的全部理由。
// 因此这里发的是一条普通的 card.update op，和用户手动编辑保存走同一条路。
//
// # 为什么改写要落进对话
//
// 版本条目的形状是 {rev, title, text, at}，装不下"这一版是按哪句指令改的"，
// 而 script 卡的 params 只认 versions 一个 key（ParseScriptParams 对未知 key
// 直接报错，就是为了防止有人在这里另起一套版本存储）。指令要是不落进对话，
// 它就彻底没了：版本列表能告诉用户第 3 版和第 4 版差在哪，却答不出为什么。
//
// 上游失败一律回错误码，卡片一个字不动：改写失败和"改成了空的"必须是
// 两件能分清的事。
func (s *server) handleRefineScriptCard(w http.ResponseWriter, r *http.Request) {
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cardID, err := pathID(r, "cardId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Instruction string `json:"instruction"`
		ModelID     string `json:"model_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "instruction", Message: "必填"}}, "参数校验未通过"))
		return
	}
	ctx := r.Context()

	snap, err := s.deps.Canvas.Snapshot(ctx, p.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	card, err := scriptCardByID(snap.Cards, cardID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	draft, trace, bill, err := s.refineScript(ctx,
		chatCall{userID: p.UserID, modelID: req.ModelID, projectID: p.ID, cardID: card.ID},
		card, instruction)
	if err != nil {
		writeError(w, r, err)
		return
	}
	logChatCall("refine", p.ID, trace)

	// model_id 一起改：这一版是这个模型写的，而卡片上只留得下最新那一版的
	// 出处。换个模型再优化一次，这一列跟着换，"当前正文是谁写的"才一直是真的。
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision, []domain.CanvasOp{{
		Type: domain.OpCardUpdate,
		ID:   card.ID,
		Patch: map[string]any{
			"title":    draft.Title,
			"text":     draft.Text,
			"model_id": trace.ModelID,
		},
	}})
	if err != nil {
		bill.refund(ctx, err)
		writeError(w, r, err)
		return
	}
	// 同步接口的成功点就在这一行：改写后的正文已经落到卡上，用户拿到了
	// 他买的东西。后面两条对话消息写不进去也不再退——那只影响记录，不影响产物。
	bill.charge(ctx)

	if _, err := s.deps.Canvases.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleUser,
		Content:    instruction,
		RefCardIDs: []string{card.ID},
		CreatedAt:  s.now(),
	}); err != nil {
		writeError(w, r, err)
		return
	}
	reply, err := s.deps.Canvases.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleAssistant,
		Content:    refineReply(draft, trace.ModelID),
		RefCardIDs: []string{card.ID},
		CreatedAt:  s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// 回的是改写后的卡片快照，而不是再拉一次全量：前端拿它就地替换那一张卡。
	// params 不在这里回——版本列表由服务端在 Apply 里追加，这个结构体是
	// 改写前读到的那一份，把它回过去等于回一个已经过期的版本列表。
	card.Title = draft.Title
	card.Text = &draft.Text
	card.ModelID = strPtrOrNil(trace.ModelID)
	card.Params = nil

	writeJSON(w, http.StatusOK, map[string]any{
		"reply_message_id": reply.ID,
		"revision":         revision,
		"card":             card,
	})
}

// scriptCardByID 从快照里挑出一张剧本卡。
//
// kind 不是 script 就明确报错而不是照改：镜头卡的正文语义完全不同
// （台词在 params.dialogue 里），把它当剧本改写会把结构化字段和正文改得
// 对不上，而那种坏数据要等到出片时才暴露。
func scriptCardByID(cards []domain.Card, cardID string) (domain.Card, error) {
	for _, c := range cards {
		if c.ID != cardID {
			continue
		}
		if c.Kind != domain.CardKindScript {
			return domain.Card{}, errInvalid(
				"卡片 %s 的 kind 是 %s，只有 script 卡能改写剧本", cardID, c.Kind)
		}
		return c, nil
	}
	return domain.Card{}, errNotFound("card")
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
