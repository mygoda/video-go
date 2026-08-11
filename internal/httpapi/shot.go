package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// shotDraft 是模型改写后的一镜正文：画面描述 + 台词，两者分列。
type shotDraft struct {
	Description string
	Dialogue   string
}

// handleRefineShotCard 按用户的一句指令让模型同时重写这一镜的画面描述与台词。
//
// 与 handleRefineScriptCard 同构，差别有二：只认镜头卡（shotCardByID），
// 写回的是 params.description / params.dialogue 而不是正文 text。镜头正文**不留
// 版本历史**（版本只对成片生效）——改坏了再润色一次或手改即可。
func (s *server) handleRefineShotCard(w http.ResponseWriter, r *http.Request) {
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
	card, err := shotCardByID(snap.Cards, cardID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	shot, err := domain.ParseShotParams(card.Params)
	if err != nil {
		writeError(w, r, err)
		return
	}

	draft, trace, bill, err := s.refineShot(ctx,
		chatCall{userID: p.UserID, modelID: req.ModelID, projectID: p.ID, cardID: card.ID},
		shot, instruction)
	if err != nil {
		writeError(w, r, err)
		return
	}
	logChatCall("refine-shot", p.ID, trace)

	// 只改这两个字段，其余（镜号、机位、景别、首帧、出场角色…）原样保留：
	// 读出来的整份 params 就地替换 description / dialogue 再写回。
	shot.Description = draft.Description
	shot.Dialogue = draft.Dialogue
	revision, err := s.deps.Canvas.Apply(ctx, p.ID, snap.Revision, []domain.CanvasOp{{
		Type:  domain.OpCardUpdate,
		ID:    card.ID,
		Patch: map[string]any{"params": shot},
	}})
	if err != nil {
		bill.refund(ctx, err)
		writeError(w, r, err)
		return
	}
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
		Content:    shotRefineReply(shot.ShotNo, trace.ModelID),
		RefCardIDs: []string{card.ID},
		CreatedAt:  s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// 前端拿到后会重新拉一次全量画布，这里回的 card 只是让它有东西可就地替换。
	writeJSON(w, http.StatusOK, map[string]any{
		"reply_message_id": reply.ID,
		"revision":         revision,
		"card":             card,
	})
}

// shotCardByID 从快照里挑出一张镜头卡。kind 不是 shot 就报错——把剧本卡 / 角色卡
// 当镜头改写会把结构化字段写坏。
func shotCardByID(cards []domain.Card, cardID string) (domain.Card, error) {
	for _, c := range cards {
		if c.ID != cardID {
			continue
		}
		if c.Kind != domain.CardKindShot {
			return domain.Card{}, errInvalid(
				"卡片 %s 的 kind 是 %s，只有 shot 卡能润色镜头", cardID, c.Kind)
		}
		return c, nil
	}
	return domain.Card{}, errNotFound("card")
}

// refineShot 按一句指令让模型重写这一镜的描述与台词。解析、计费与 refineScript
// 复用同一套（chatOnce），只有提示词与产物解析不同。
func (s *server) refineShot(ctx context.Context, call chatCall, shot domain.ShotParams, instruction string) (shotDraft, chatTrace, *chatBill, error) {
	call.step = "refine-shot"
	call.prompt = shotRefinePrompt(shot, instruction)
	call.userInput = instruction

	reply, trace, bill, err := s.chatOnce(ctx, call)
	if err != nil {
		return shotDraft{}, trace, nil, err
	}
	draft, err := parseShotReply(reply)
	if err != nil {
		bill.refund(ctx, err)
		return shotDraft{}, trace, nil, err
	}
	return draft, trace, bill, nil
}

// shotRefinePrompt 拼出改写用的那段话：把这一镜现有的画面描述与台词发上去，
// 连同用户指令，要求模型按固定的两段格式回，好让 parseShotReply 稳定解析。
func shotRefinePrompt(shot domain.ShotParams, instruction string) string {
	var b strings.Builder
	b.WriteString("你是一位短片分镜师。下面是一个镜头的画面描述与台词，请按用户的修改要求改写。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. **只改用户要求改的部分**，其余原样保留。\n")
	b.WriteString("2. 画面描述只写这一镜看得见的画面（环境、人物动作、镜头运动），**不要把台词写进描述**。\n")
	b.WriteString("3. 台词只写这一镜里人物说的话；如果这是空镜 / 没有台词，台词那一行留空。\n")
	b.WriteString("4. **严格按下面两行格式输出**，不要加任何解释、点评或代码块：\n")
	b.WriteString("描述：<改写后的画面描述>\n")
	b.WriteString("台词：<改写后的台词，可为空>\n")

	b.WriteString("\n原画面描述：\n")
	b.WriteString(strings.TrimSpace(shot.Description))
	b.WriteString("\n\n原台词：\n")
	b.WriteString(strings.TrimSpace(shot.Dialogue))
	b.WriteString("\n\n修改要求：\n")
	b.WriteString(instruction)
	return b.String()
}

// parseShotReply 解析「描述：… / 台词：…」两段格式。半角/全角冒号都认，
// 台词外层的「」若被模型带上一并剥掉（台词在库里不含引号，引号是 UI 加的）。
func parseShotReply(reply string) (shotDraft, error) {
	body := strings.TrimSpace(reply)
	if body == "" {
		return shotDraft{}, &domain.Error{
			Code: domain.CodeInternal, Message: "镜头润色模型返回成功但内容是空的", Retryable: true,
		}
	}

	desc, dialogue := splitShotReply(body)
	desc = strings.TrimSpace(desc)
	dialogue = strings.Trim(strings.TrimSpace(dialogue), "「」\"'")
	if desc == "" {
		return shotDraft{}, &domain.Error{
			Code: domain.CodeInternal, Message: "镜头润色没解析出画面描述", Retryable: true,
		}
	}
	return shotDraft{Description: desc, Dialogue: dialogue}, nil
}

// splitShotReply 把回复切成描述段与台词段：以「台词：」为界，之前是描述
// （去掉可能的「描述：」前缀），之后是台词。没有「台词：」标记时整段当描述。
func splitShotReply(body string) (desc, dialogue string) {
	marker := ""
	for _, m := range []string{"台词：", "台词:"} {
		if i := strings.Index(body, m); i >= 0 {
			marker = m
			break
		}
	}
	if marker == "" {
		return stripLabel(body), ""
	}
	i := strings.Index(body, marker)
	return stripLabel(body[:i]), body[i+len(marker):]
}

// stripLabel 去掉描述段开头可能的「描述：」/「画面：」前缀。
func stripLabel(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"描述：", "描述:", "画面：", "画面:"} {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}

// shotRefineReply 是镜头润色落进对话里的那条助手消息。
func shotRefineReply(shotNo int, modelID string) string {
	return "已用 " + modelID + " 润色第 " + strconv.Itoa(shotNo) + " 镜的画面描述与台词，就在那张镜头卡上，可以直接改。"
}
