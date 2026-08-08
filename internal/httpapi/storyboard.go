// 画布对话的分镜拆解：把用户发来的一段脚本交给 chat 模型拆成若干镜头，
// 每个镜头落成画布上的一张文本卡片。
//
// # 为什么是同步做完直接返回卡片，而不是派一串任务
//
// 拆分镜的产物是**文字**，chat 族本身就是一次 HTTP 往返拿结果（见
// domain.ProtocolFamily 的注释）。为它建 task 行、冻结积分、走 worker、
// 再从 SSE 推回来，只是把一次几秒的调用包装成一条永远不会失败得更少的
// 异步链路，还要用户盯着转圈。因此这里直接调 driver、直接落卡片、
// 直接把新卡片回给前端；task_ids 保持空数组，因为**确实没有任务**——
// 那和以前"恒为空数组假装派了任务"是两回事。
//
// # 为什么不写死模型
//
// 选模型走 ModelConfig：优先取配置里的默认分镜模型（AIGC_STORYBOARD_MODEL），
// 没配就取第一个启用的 chat 族模型。写死一个 id 意味着换供应商要改代码发版，
// 而"改配置不重启不发版"正是 models 表存在的理由。
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

const (
	// storyboardMaxShots 是一次拆解最多落多少张卡片。上界不是性能考虑，
	// 是防止模型把一句话拆成四十个镜头把画布糊满。
	storyboardMaxShots = 12

	// 新卡片的排布参数。文本卡片按行铺开，宽高与前端文本卡的默认尺寸同量级。
	storyboardCardW  = 280.0
	storyboardCardH  = 180.0
	storyboardGap    = 32.0
	storyboardPerRow = 4
)

// storyboardShot 是模型返回的一个镜头。字段名就是喂给模型的 JSON 契约，
// 改这里必须同时改 storyboardPrompt 里的示例。
type storyboardShot struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// storyboardTrace 是一次分镜调用的摘要，用于日志与排查。
// **不含凭证，也不含完整响应体**——只留够回答"这次是不是真的打到上游了"。
type storyboardTrace struct {
	ModelID       string
	UpstreamModel string
	ProviderID    string
	PromptChars   int
	ReplyChars    int
	Shots         int
	LatencyMS     int64
}

// storyboardModel 选出这次分镜要用的模型。
//
// 配置里指定了就用指定的那个（并检查它确实是 chat 族——配错时明确报错，
// 好过拿一个 video 模型去发 chat 请求然后在上游那边失败）。
func (s *server) storyboardModel(ctx context.Context) (domain.ModelConfig, error) {
	if id := strings.TrimSpace(s.deps.Config.StoryboardModelID); id != "" {
		m, err := s.deps.Models.Get(ctx, id)
		if err != nil {
			return domain.ModelConfig{}, err
		}
		if !m.Enabled {
			return domain.ModelConfig{}, errInvalid("配置的分镜模型 %s 当前已禁用", id)
		}
		if m.Family != domain.FamilyChat {
			return domain.ModelConfig{}, errInvalid(
				"配置的分镜模型 %s 的 protocol_family 是 %s，分镜需要 chat", id, m.Family)
		}
		return m, nil
	}

	enabled := true
	models, err := s.deps.Models.List(ctx, domain.ModelFilter{Enabled: &enabled})
	if err != nil {
		return domain.ModelConfig{}, err
	}
	for _, m := range models {
		if m.Family == domain.FamilyChat {
			return m, nil
		}
	}
	return domain.ModelConfig{}, &domain.Error{
		Code: domain.CodeInvalidParam,
		Message: "没有可用的 chat 模型，无法拆分镜；" +
			"请在管理后台启用一个 protocol_family=chat 的模型，或设置 AIGC_STORYBOARD_MODEL",
	}
}

// storyboard 调一次 chat 上游，把脚本拆成镜头。
//
// refs 是用户在对话里勾选的卡片，它们的标题与正文作为上下文一起发上去——
// 「多卡引用为下次输入」如果不真的进入提示词，那个勾选框就只是个装饰。
func (s *server) storyboard(ctx context.Context, script string, refs []domain.Card) ([]storyboardShot, storyboardTrace, error) {
	model, err := s.storyboardModel(ctx)
	if err != nil {
		return nil, storyboardTrace{}, err
	}
	provider, err := s.deps.Providers.Get(ctx, model.ProviderID)
	if err != nil {
		return nil, storyboardTrace{}, err
	}
	if !provider.Enabled {
		return nil, storyboardTrace{}, errInvalid("供应商 %s 当前已禁用", provider.Name)
	}
	credential := os.Getenv(provider.CredentialRef)
	if strings.TrimSpace(credential) == "" {
		return nil, storyboardTrace{}, errInvalid(
			"环境变量 %s 未设置，无法调用分镜模型 %s", provider.CredentialRef, model.ID)
	}

	schema, err := capability.DecodeSchema(model.Capability)
	if err != nil {
		return nil, storyboardTrace{}, errInternal(err)
	}

	prompt := storyboardPrompt(script, refs)
	in := adapter.SubmitInput{
		TaskID:         "storyboard-" + uid.Token(8),
		Provider:       provider,
		Model:          model,
		UpstreamModel:  model.UpstreamModel,
		Prompt:         prompt,
		Params:         defaultParams(schema),
		Credential:     credential,
		IdempotencyKey: "storyboard-" + uid.Token(8),
	}

	driver, err := adapter.ResolveSync(s.deps.Drivers, model)
	if err != nil {
		return nil, storyboardTrace{}, errInternal(err)
	}

	// 超时取供应商配置，与 executor.invoke 同一套规则：一个卡住的上游
	// 不该把 HTTP handler 一起挂住。
	cctx := ctx
	if provider.TimeoutMS > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(provider.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	start := time.Now()
	out, err := driver.Invoke(cctx, in)
	trace := storyboardTrace{
		ModelID:       model.ID,
		UpstreamModel: model.UpstreamModel,
		ProviderID:    provider.ID,
		PromptChars:   len([]rune(prompt)),
		LatencyMS:     time.Since(start).Milliseconds(),
	}
	if err != nil {
		// driver 返回的已经是带分类码的 *domain.Error，原样上抛即可：
		// 包一层 internal error 会把 upstream_rate_limited 这类可重试信息抹掉。
		return nil, trace, err
	}

	reply, err := storyboardReplyText(out)
	if err != nil {
		return nil, trace, err
	}
	trace.ReplyChars = len([]rune(reply))

	shots, err := parseStoryboardShots(reply)
	if err != nil {
		return nil, trace, err
	}
	trace.Shots = len(shots)
	return shots, trace, nil
}

// storyboardReplyText 把 driver 回来的产物还原成文本。
//
// chat 族的产物是 KindBase64 + text/plain（见 openaicompat.chatArtifacts），
// 这里按那个形状解，不认识的形状明确报错而不是当空字符串继续走——
// 静默的空回复会变成"画布上什么也没多出来"，那正是这次要修掉的症状。
func storyboardReplyText(out adapter.SubmitResult) (string, error) {
	for _, a := range out.Inline {
		if a.Type != domain.AssetTypeText {
			continue
		}
		switch a.Kind {
		case adapter.KindBase64:
			raw, err := base64.StdEncoding.DecodeString(a.Base64)
			if err != nil {
				return "", errInternal(fmt.Errorf("分镜回复的 base64 解码失败: %w", err))
			}
			if text := strings.TrimSpace(string(raw)); text != "" {
				return text, nil
			}
		case adapter.KindURL:
			return "", errInvalid("分镜模型返回的是 URL 形态的文本产物，暂不支持")
		}
	}
	return "", &domain.Error{
		Code:      domain.CodeInternal,
		Message:   "分镜模型返回成功但没有可用的文本内容",
		Retryable: true,
	}
}

// storyboardPrompt 拼出发给模型的那段话。
//
// 要求模型只回 JSON 数组：拿自然语言回来还得再写一个中文分镜格式的解析器，
// 而那个解析器会在模型换一种排版时立刻失效。JSON 至少是可判定的——
// 解析失败就是解析失败，不会似是而非地拆出半截。
func storyboardPrompt(script string, refs []domain.Card) string {
	var b strings.Builder
	b.WriteString("你是一位分镜师。把下面这段内容拆成若干个连续的镜头。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 只输出一个 JSON 数组，不要输出任何解释、前后缀或代码块标记。\n")
	b.WriteString("2. 数组每一项形如 {\"title\": \"镜头标题\", \"description\": \"这个镜头的画面描述\"}。\n")
	b.WriteString("3. title 控制在 12 个字以内，description 是可以直接用于文生图的画面描述，")
	b.WriteString("包含主体、动作、环境、镜头语言。\n")
	b.WriteString(fmt.Sprintf("4. 镜头数量按内容长度自行决定，最多 %d 个。\n", storyboardMaxShots))

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

	b.WriteString("\n待拆解的内容：\n")
	b.WriteString(script)
	return b.String()
}

// parseStoryboardShots 从模型回复里取出镜头数组。
//
// 模型很爱把 JSON 包在 ```json 围栏里，也很爱在数组前后加一句"好的，以下是"，
// 因此这里从第一个 '[' 开始读，忽略前面的客套话。
//
// 逐个元素流式解码而不是整段 Unmarshal：max_tokens 到顶时上游会在半个对象
// 中间把话截断，整段解会因为最后那半个对象作废，把前面十个完整镜头一起丢掉。
// 流式解到解不动为止，保住已经完整的部分。
//
// 一个都解不出来就报错，**不做任何模板兜底**——凭空造几张卡片会让人
// 以为模型工作了。
func parseStoryboardShots(reply string) ([]storyboardShot, error) {
	start := strings.Index(reply, "[")
	if start < 0 {
		return nil, storyboardParseError(reply)
	}

	dec := json.NewDecoder(strings.NewReader(reply[start:]))
	if _, err := dec.Token(); err != nil { // 吃掉开头的 '['
		return nil, storyboardParseError(reply)
	}

	out := make([]storyboardShot, 0, storyboardMaxShots)
	for dec.More() && len(out) < storyboardMaxShots {
		var sh storyboardShot
		if err := dec.Decode(&sh); err != nil {
			break
		}
		desc := strings.TrimSpace(sh.Description)
		if desc == "" {
			continue
		}
		title := strings.TrimSpace(sh.Title)
		if title == "" {
			title = fmt.Sprintf("镜头 %d", len(out)+1)
		}
		out = append(out, storyboardShot{Title: title, Description: desc})
	}
	if len(out) == 0 {
		return nil, storyboardParseError(reply)
	}
	return out, nil
}

// storyboardParseError 把上游原文的头尾都带上。
//
// 只带开头不够用：解析失败最常见的原因就是被 max_tokens 截断，而截断的
// 证据只在末尾。带上长度是为了一眼分辨"模型没按格式说话"和"话没说完"。
func storyboardParseError(reply string) error {
	head := truncateRunes(reply, 160)
	tail := ""
	if r := []rune(reply); len(r) > 320 {
		tail = "…" + string(r[len(r)-160:])
	}
	return &domain.Error{
		Code: domain.CodeInternal,
		Message: fmt.Sprintf("分镜模型的回复不是预期的 JSON 数组，无法拆成镜头（共 %d 字）；原文：%s%s",
			len([]rune(reply)), head, tail),
		Retryable: true,
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// storyboardCards 把镜头铺成卡片。
//
// 起始纵坐标取现有卡片的最低边之下，新的一批不会盖在旧卡片上；
// AutoPlaced 置 true，用户手动挪过之后前端会把它翻成 false，
// 那一片区域就退出自动重排。
func storyboardCards(shots []storyboardShot, existing []domain.Card, refs []string, now time.Time) []domain.Card {
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
		baseY += storyboardGap * 2
	}
	if refs == nil {
		refs = []string{}
	}

	cards := make([]domain.Card, 0, len(shots))
	for i, sh := range shots {
		row := i / storyboardPerRow
		col := i % storyboardPerRow
		text := sh.Description
		cards = append(cards, domain.Card{
			ID:         uid.New(),
			Kind:       domain.CardKindText,
			Title:      sh.Title,
			X:          float64(col) * (storyboardCardW + storyboardGap),
			Y:          baseY + float64(row)*(storyboardCardH+storyboardGap),
			W:          storyboardCardW,
			H:          storyboardCardH,
			Z:          topZ + float64(i+1),
			Text:       &text,
			Refs:       refs,
			History:    []domain.CardVersion{},
			AutoPlaced: true,
			CreatedAt:  now,
		})
	}
	return cards
}

// logStoryboard 记一条可核对的调用摘要。
//
// 只记模型、耗时、字数、镜头数——够回答"这次到底有没有打到上游、
// 上游说了多少话"，又不会把用户脚本原文写进日志。
func logStoryboard(projectID string, t storyboardTrace) {
	slog.Info("画布分镜完成",
		"project_id", projectID,
		"model_id", t.ModelID,
		"upstream_model", t.UpstreamModel,
		"provider_id", t.ProviderID,
		"prompt_chars", t.PromptChars,
		"reply_chars", t.ReplyChars,
		"shots", t.Shots,
		"latency_ms", t.LatencyMS)
}
