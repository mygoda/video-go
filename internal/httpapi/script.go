// 画布对话的第一步：把用户发来的一句话变成一份**剧本**，落成画布上的一张
// script 卡。这一步到此为止——不拆镜头，不派任何出片任务，不花一分钱。
//
// # 为什么要单独停在这里
//
// 在此之前，用户发一句话会被一次不可见的模型调用直接吞成 3 张视频卡，
// 每张卡立刻起一条真的视频任务：用户还没看到任何东西，钱已经花出去了，
// 而中间的剧本、分镜两层根本没有作为对象存在过——看不见，也就改不动。
// 拆分镜是下一步的事，出片还要更后面；每一层都得先停下来让用户看一眼。
//
// # 为什么剧本回复要纯文本，不要 JSON
//
// 分镜那一步要的是一个数组，元素多、每个都短，JSON 是可判定的（见
// storyboard.go）。剧本正好相反：一整篇带换行的长中文散文，模型往 JSON
// 字符串里塞裸换行是常态，一次解析失败就把整篇剧本连同这次调用一起作废。
// 纯文本没有这种失败模式——正文原样就是正文，只有标题需要约定，
// 而"第一行是标题"是模型最难写错的一种约定。
package httpapi

import (
	"context"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

const (
	// 剧本卡的默认尺寸。比镜头卡大一圈：它装的是一整篇正文，
	// 按镜头卡的 280×180 铺出来，一屏只能看见开头两行。
	scriptCardW = 420.0
	scriptCardH = 360.0

	// scriptTitleMaxRunes 是剧本标题的截断长度。模型偶尔会把整个第一段
	// 当标题写回来，截一刀好过让卡片标题栏撑爆。
	scriptTitleMaxRunes = 24

	// canvasCardGap 是自动排布时新卡片与既有卡片之间留的间距。
	canvasCardGap = 32.0
)

// scriptDraft 是模型产出的一份剧本。
type scriptDraft struct {
	Title string
	Text  string
}

// generateScript 调一次 chat 上游，把用户的一句话写成剧本。
//
// refs 是用户在对话里勾选的卡片，它们的标题与正文作为上下文一起发上去——
// 「多卡引用为下次输入」如果不真的进入提示词，那个勾选框就只是个装饰。
//
// modelID 是用户这次点名的模型，空串表示按默认规则选。
func (s *server) generateScript(ctx context.Context, idea string, refs []domain.Card, modelID string) (scriptDraft, chatTrace, error) {
	reply, trace, err := s.chatOnce(ctx, "script", modelID, scriptPrompt(idea, refs))
	if err != nil {
		return scriptDraft{}, trace, err
	}
	draft, err := parseScriptReply(reply)
	if err != nil {
		return scriptDraft{}, trace, err
	}
	return draft, trace, nil
}

// scriptPrompt 拼出发给模型的那段话。
//
// 明确写"不要分镜、不要镜号"：模型见到"短剧"两个字的默认反应就是直接给
// 分镜表，而那正是这一步要推迟的东西——分镜得等用户先把剧本改顺了再拆。
func scriptPrompt(idea string, refs []domain.Card) string {
	var b strings.Builder
	b.WriteString("你是一位短剧编剧。把下面这个想法写成一份完整的短剧剧本。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 第一行只写剧本标题，不要加「标题」「片名」之类的前缀，也不要加书名号。\n")
	b.WriteString("2. 从第二行开始写正文：场景、人物、动作、台词，按场次组织。\n")
	b.WriteString("3. **不要拆分镜，不要写镜号、景别、机位、时长**——分镜是下一步的事。\n")
	b.WriteString("4. 只输出剧本本身，不要输出解释、点评或代码块标记。\n")

	if len(refs) > 0 {
		b.WriteString("\n已有画布卡片（作为上下文参考，风格与设定要与它们保持一致）：\n")
		for _, c := range refs {
			title := c.Title
			if title == "" {
				title = "（未命名）"
			}
			b.WriteString("- " + title)
			if c.Text != nil && strings.TrimSpace(*c.Text) != "" {
				b.WriteString("：" + truncateRunes(strings.TrimSpace(*c.Text), 200))
			} else if c.Prompt != nil && strings.TrimSpace(*c.Prompt) != "" {
				b.WriteString("：" + truncateRunes(strings.TrimSpace(*c.Prompt), 200))
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n想法：\n")
	b.WriteString(idea)
	return b.String()
}

// parseScriptReply 把模型回复切成标题与正文。
//
// 第一个非空行是标题，其余是正文。模型爱给标题加 Markdown 井号或「标题：」
// 之类的前缀，这里剥掉——留着的话卡片标题栏会显示成「## 雨夜重逢」。
//
// 只有一行时不硬拆：那一行既是标题也是正文（模型给了一句话梗概）。
// 把正文清空只会让卡片看着像生成失败了，而它其实没有。
//
// 一个字都没有就报错，**不做任何模板兜底**——凭空造一份剧本会让人
// 以为模型工作了。
func parseScriptReply(reply string) (scriptDraft, error) {
	body := strings.TrimSpace(reply)
	if body == "" {
		return scriptDraft{}, &domain.Error{
			Code:      domain.CodeInternal,
			Message:   "剧本模型返回成功但正文是空的",
			Retryable: true,
		}
	}

	head, rest, _ := strings.Cut(body, "\n")
	title := cleanScriptTitle(head)
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return scriptDraft{Title: title, Text: body}, nil
	}
	return scriptDraft{Title: title, Text: rest}, nil
}

// cleanScriptTitle 把标题行剥成干净的一句。
func cleanScriptTitle(line string) string {
	title := strings.TrimSpace(line)
	title = strings.TrimLeft(title, "#＃ 　")
	for _, prefix := range []string{"标题：", "标题:", "片名：", "片名:"} {
		title = strings.TrimPrefix(title, prefix)
	}
	title = strings.Trim(strings.TrimSpace(title), "《》\"“”")
	if title == "" {
		return "未命名剧本"
	}
	return truncateRunes(title, scriptTitleMaxRunes)
}

// scriptCard 把一份剧本铺成画布上的一张卡片。
//
// 起始纵坐标取现有卡片的最低边之下，新卡片不会盖在旧卡片上；AutoPlaced
// 置 true，用户手动挪过之后前端会把它翻成 false，那一片区域就退出自动重排。
//
// Params 显式给一个空的版本列表而不是留 nil：前端读 params.versions.length
// 显示「共 N 版」，少这一个字段就是一次 undefined。
//
// modelID 记的是**写出这一版正文的那个模型**。此前这一列在所有 script 卡上
// 恒为 NULL，因为写剧本用哪个模型是全局配置、不是每张卡的属性。现在模型
// 可以逐次点名，"这一版是谁写的"就成了卡片自身的事实：不留下它，用户换个
// 模型重写一版之后没有任何办法把两版和各自的模型对上。
func scriptCard(draft scriptDraft, existing []domain.Card, refs []string, modelID string, now time.Time) domain.Card {
	baseY := 0.0
	topZ := 0.0
	for _, c := range existing {
		if bottom := c.Y + c.H; bottom > baseY {
			baseY = bottom
		}
		if c.Z > topZ {
			topZ = c.Z
		}
	}
	if len(existing) > 0 {
		baseY += canvasCardGap * 2
	}
	if refs == nil {
		refs = []string{}
	}

	text := draft.Text
	return domain.Card{
		ID:         uid.New(),
		Kind:       domain.CardKindScript,
		Title:      draft.Title,
		X:          0,
		Y:          baseY,
		W:          scriptCardW,
		H:          scriptCardH,
		Z:          topZ + 1,
		Text:       &text,
		ModelID:    strPtrOrNil(modelID),
		Params:     map[string]any{"versions": []domain.ScriptVersion{}},
		Refs:       refs,
		History:    []domain.CardVersion{},
		AutoPlaced: true,
		CreatedAt:  now,
	}
}

// scriptReply 是落进对话里的那条助手消息。
//
// 明说"没有生成任何画面"：上一版这里会派一批出片任务，用户对这个接口的
// 既有预期就是"发一句话就开始出片"。不点破的话，用户会盯着画布等一批
// 永远不会出现的视频卡。
func scriptReply(draft scriptDraft, modelID string) string {
	return "已用 " + modelID + " 写好剧本《" + draft.Title + "》，" +
		"就在画布上，可以直接改。这一步只出剧本，没有生成任何画面；" +
		"改顺了之后再拆分镜。"
}

// refineScript 按用户的一句指令让模型重写一遍剧本。
//
// 它与 generateScript 的差别只在提示词：产物同样是"第一行标题 + 正文"，
// 因此解析、落卡、留版本全部复用同一套路径。
func (s *server) refineScript(ctx context.Context, card domain.Card, instruction, modelID string) (scriptDraft, chatTrace, error) {
	reply, trace, err := s.chatOnce(ctx, "refine", modelID, refinePrompt(card, instruction))
	if err != nil {
		return scriptDraft{}, trace, err
	}
	draft, err := parseScriptReply(reply)
	if err != nil {
		return scriptDraft{}, trace, err
	}
	return draft, trace, nil
}

// refinePrompt 拼出改写用的那段话。
//
// # 为什么要把整篇旧正文发上去，而不是只发指令
//
// chat 调用是无状态的，上游那边不存在"这张卡是什么"。不带原文的"把主角换
// 成女性"只会让模型凭空另写一篇——用户看到的是剧本被换掉了，不是被改了。
//
// # 为什么反复强调"只改指令要求的部分"
//
// 模型拿到一整篇正文加一句指令，默认反应是顺手把它认为不够好的地方一起
// 重写。而用户点「优化」时脑子里想的是一次定向修改：改完还要能和上一版
// 逐段对照，通篇换掉就没法对照了。旧版本虽然留在 params.versions 里丢不了，
// 但"每改一句话就等于重写一遍"会让这个按钮没人敢按。
//
// 输出格式的要求与 scriptPrompt 一字不差（第一行标题、不拆分镜、不要解释），
// 因为两边的产物要喂给同一个 parseScriptReply。
func refinePrompt(card domain.Card, instruction string) string {
	var b strings.Builder
	b.WriteString("你是一位短剧编剧。下面是一份已有的短剧剧本，请按用户的修改要求改写它。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. **只改用户要求改的部分**，其余的场景、人物、台词、结构一律原样保留。\n")
	b.WriteString("2. 输出**完整的**改写后剧本，不要只输出改动的片段，也不要写「其余不变」之类的省略。\n")
	b.WriteString("3. 第一行只写剧本标题，不要加「标题」「片名」之类的前缀，也不要加书名号。" +
		"改写要求没提到标题就沿用原标题。\n")
	b.WriteString("4. 从第二行开始写正文。\n")
	b.WriteString("5. **不要拆分镜，不要写镜号、景别、机位、时长**——分镜是下一步的事。\n")
	b.WriteString("6. 只输出剧本本身，不要输出解释、点评、改动说明或代码块标记。\n")

	title := card.Title
	if title == "" {
		title = "（未命名）"
	}
	b.WriteString("\n原剧本标题：\n")
	b.WriteString(title)
	b.WriteString("\n\n原剧本正文：\n")
	if card.Text != nil {
		b.WriteString(strings.TrimSpace(*card.Text))
	}
	b.WriteString("\n\n修改要求：\n")
	b.WriteString(instruction)
	return b.String()
}

// refineReply 是改写落进对话里的那条助手消息。
//
// 明说旧版本还在：用户点「优化」时最真实的顾虑是"改坏了怎么办"，
// 而版本机制已经兜住了这件事，只是它默默发生在服务端，不说没人知道。
func refineReply(draft scriptDraft, modelID string) string {
	return "已用 " + modelID + " 改好剧本《" + draft.Title + "》，" +
		"改动前的那一版留在卡片的版本列表里，随时能切回去。"
}
