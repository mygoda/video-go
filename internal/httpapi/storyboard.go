// 分镜拆解：把一份剧本交给 chat 模型拆成若干镜头，落成画布上的 N 张 shot 卡。
//
// # 这一步只落文字
//
// 拆解本身同步：产物是文字，chat 族一次 HTTP 往返就有结果，包装成异步任务
// 链路只是让用户多盯一会儿转圈（见 chatcall.go）。**拆解不出图也不出片。**
// 上一版在这里为每个镜头立刻派一条真视频任务，用户还没看见任何东西钱就花
// 出去了——那条派发链路已经整个删掉，出片是用户看过、改过镜头之后另一步
// 的事。这一步跑完，`tasks` 表除了那一次 chat 调用之外一行不增。
//
// # 由用户在剧本卡上主动触发，不自动往下走
//
// 剧本那一步（script.go）结束后画布上停着一张剧本卡，链路就断在那里等人。
// 分镜是紧随其后的独立一步，入口是 POST /api/canvas/{projectId}/storyboard，
// 必须由用户点着某张剧本卡发起。每一层都得先停下来让用户看一眼、改一改。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

const (
	// storyboardMaxShots 是一次拆解最多落多少张卡片，也是入参的硬上限。
	// 上界不是性能考虑，是防止模型把一句话拆成四十个镜头把画布糊满。
	//
	// 超上限**报错而不是静默截断**：用户要 20 个镜头却拿回 12 个，画布上
	// 少掉的那 8 个没有任何痕迹，他只会以为模型没拆完。
	storyboardMaxShots = 12

	// storyboardDefaultShots 是没传镜头数时提示词里要求模型拆出的镜头数。
	//
	// 取 3 的理由仍然是钱：每个镜头最终都要真调一次视频模型（一段 4 秒 480p
	// 约 80 秒、要花真金白银），拆得越多，用户在下一步一键出片时的账单越长。
	// 3 段足够呈现起承转合，也是"先看一眼再决定"最省的起点。
	//
	// 但**闸门不能只由这个常量来把**：它对用户不可见，想要 5 个镜头的人在
	// 界面上找不到任何地方可以说这句话。因此它现在只是默认值——真正的闸门是
	// storyboardMaxShots，而在两者之间用户想要几个就是几个，那是他主动选择
	// 多花的钱。
	//
	// 镜头数写进提示词而不是"拆 12 个再砍到 N 个"：砍掉的那些镜头模型已经
	// 算过、token 已经花过，而且被砍掉的往往正是结尾，留下的几段讲不完一个故事。
	storyboardDefaultShots = 3

	// 镜头卡的默认尺寸。比剧本卡小一圈：它装的是一句画面描述加一句台词，
	// 按剧本卡的 420×360 铺出来，一屏放不下三个镜头。
	shotCardW = 280.0
	shotCardH = 180.0
)

// storyboardShot 是模型返回的一个镜头。字段名就是喂给模型的 JSON 契约，
// 改这里必须同时改 storyboardPrompt 里的示例。
//
// Dialogue 与 Description 分开，是因为落进 shot 卡时它们本来就分属两个字段
// （见 domain.ShotParams 的注释：出片时台词进 prompt 才有同步人声，字幕也
// 直接取台词字段）。在模型这一层就分开要，好过拿回一整段再用正则去抠引号。
type storyboardShot struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Dialogue    string `json:"dialogue"`
}

// generateStoryboard 调一次 chat 上游，把一份剧本拆成 shots 个镜头。
func (s *server) generateStoryboard(ctx context.Context, script string, shots int, refs []domain.Card) ([]storyboardShot, chatTrace, error) {
	reply, trace, err := s.chatOnce(ctx, "storyboard", storyboardPrompt(script, shots, refs))
	if err != nil {
		return nil, trace, err
	}
	out, err := parseStoryboardShots(reply, shots)
	if err != nil {
		return nil, trace, err
	}
	return out, trace, nil
}

// resolveShotCount 把请求里的镜头数收敛成一个合法值。
//
// 用指针区分"没传"和"传了 0"：前者取默认值，后者是用户明确要了一个拆不出
// 任何东西的数，得当场顶回去而不是悄悄按 3 拆——他会以为自己传的那个数生效了。
func resolveShotCount(want *int) (int, error) {
	if want == nil {
		return storyboardDefaultShots, nil
	}
	n := *want
	if n < 1 {
		return 0, errFields([]domain.FieldError{{
			Key:     "shot_count",
			Message: fmt.Sprintf("镜头数至少 1（传入 %d）", n),
		}}, "参数校验未通过")
	}
	if n > storyboardMaxShots {
		return 0, errFields([]domain.FieldError{{
			Key: "shot_count",
			Message: fmt.Sprintf("镜头数最多 %d（传入 %d）；镜头越多下一步出片越贵，"+
				"要更长的片子请先拆到上限，再对不够的部分单独补拆", storyboardMaxShots, n),
		}}, "参数校验未通过")
	}
	return n, nil
}

// storyboardSource 从画布快照里取出要拆的那张剧本卡。
//
// 三种情况分别报清楚：卡片不在这张画布上、它不是剧本卡、它还没有正文。
// 回一句"参数错误"等于让用户挨个点卡片试。
func storyboardSource(cards []domain.Card, cardID string) (domain.Card, error) {
	for _, c := range cards {
		if c.ID != cardID {
			continue
		}
		if c.Kind != domain.CardKindScript {
			return domain.Card{}, errFields([]domain.FieldError{{
				Key:     "card_id",
				Message: fmt.Sprintf("只能从剧本卡拆分镜，这张卡是 %s", c.Kind),
			}}, "参数校验未通过")
		}
		if c.Text == nil || strings.TrimSpace(*c.Text) == "" {
			return domain.Card{}, errFields([]domain.FieldError{{
				Key:     "card_id",
				Message: "这张剧本卡还没有正文，先把剧本写出来再拆分镜",
			}}, "参数校验未通过")
		}
		return c, nil
	}
	return domain.Card{}, errFields([]domain.FieldError{{
		Key:     "card_id",
		Message: "这张卡片不在本画布上：" + cardID,
	}}, "参数校验未通过")
}

// storyboardPrompt 拼出发给模型的那段话。
//
// 要求模型只回 JSON 数组：拿自然语言回来还得再写一个中文分镜格式的解析器，
// 而那个解析器会在模型换一种排版时立刻失效。JSON 至少是可判定的——
// 解析失败就是解析失败，不会似是而非地拆出半截。
//
// （剧本那一步反过来要纯文本，理由见 script.go 的包内注释：一整篇带换行的
// 散文塞进 JSON 字符串，模型漏转义一次就整篇作废。）
func storyboardPrompt(script string, shots int, refs []domain.Card) string {
	var b strings.Builder
	b.WriteString("你是一位分镜师。把下面这段内容拆成若干个连续的镜头。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 只输出一个 JSON 数组，不要输出任何解释、前后缀或代码块标记。\n")
	b.WriteString("2. 数组每一项形如 " +
		"{\"title\": \"镜头标题\", \"description\": \"这个镜头的画面描述\", \"dialogue\": \"这个镜头里的台词\"}。\n")
	b.WriteString("3. title 控制在 12 个字以内，description 是可以直接用于文生视频的画面描述，")
	b.WriteString("包含主体、动作、环境、镜头语言。\n")
	// 台词单独成字段是 shot 卡 schema 的核心（见 domain.ShotParams）：并进
	// description 之后出片时拼不出人声、也生成不了字幕，而那正是原始故障。
	b.WriteString("4. **台词一律写进 dialogue，不要写进 description**：description 里只描述画面，")
	b.WriteString("不要出现对白原文，也不要出现「他说」这类转述。")
	b.WriteString("这一镜没有台词（空镜、纯环境音）就把 dialogue 留成空字符串。\n")
	b.WriteString(fmt.Sprintf("5. 正好拆 %d 个镜头，不多不少；如果内容很长，就挑最关键的 %d 个画面。\n",
		shots, shots))
	b.WriteString("6. 数组顺序就是镜号顺序，按剧情从头推到尾。\n")

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

// parseStoryboardShots 从模型回复里取出镜头数组，最多取 want 个。
//
// 模型很爱把 JSON 包在 ```json 围栏里，也很爱在数组前后加一句"好的，以下是"，
// 因此这里从第一个 '[' 开始读，忽略前面的客套话。
//
// 逐个元素流式解码而不是整段 Unmarshal：max_tokens 到顶时上游会在半个对象
// 中间把话截断，整段解会因为最后那半个对象作废，把前面十个完整镜头一起丢掉。
// 流式解到解不动为止，保住已经完整的部分。
//
// want 是用户要的镜头数：模型多给了就丢掉多的（用户要 3 个就该拿到 3 个），
// 少给了不补——凭空造卡片会让人以为模型工作了。want 越界时退回硬上限，
// 这条兜底只为"调用方忘了先过 resolveShotCount"，正常路径上两者相等。
//
// 一个都解不出来就报错，**不做任何模板兜底**。
func parseStoryboardShots(reply string, want int) ([]storyboardShot, error) {
	limit := want
	if limit < 1 || limit > storyboardMaxShots {
		limit = storyboardMaxShots
	}

	start := strings.Index(reply, "[")
	if start < 0 {
		return nil, storyboardParseError(reply)
	}

	dec := json.NewDecoder(strings.NewReader(reply[start:]))
	if _, err := dec.Token(); err != nil { // 吃掉开头的 '['
		return nil, storyboardParseError(reply)
	}

	out := make([]storyboardShot, 0, limit)
	for dec.More() && len(out) < limit {
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
		out = append(out, storyboardShot{
			Title:       title,
			Description: desc,
			Dialogue:    strings.TrimSpace(sh.Dialogue),
		})
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

// shotCards 把一组镜头铺成画布上的一排卡片。
//
// 镜号按数组顺序从 1 编起，横向依次排开：**镜号是拼片顺序**（见
// domain.ShotParams），它必须来自这里的下标，不能取模型自己写的编号——
// 模型偶尔会从 0 开始，或者跳号。
//
// 起始纵坐标取现有卡片的最低边之下，新卡片不会盖在旧卡片上；真正好看的
// 排布是 T4 的事，这里只保证不重叠、且从左到右就是镜号顺序。
//
// Refs 指回来源剧本卡：这是**血缘**，「这几张镜头卡是哪份剧本拆出来的」
// 全靠它，而不是靠用户拉线。
func shotCards(shots []storyboardShot, scriptCardID string, existing []domain.Card, now time.Time) []domain.Card {
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

	out := make([]domain.Card, 0, len(shots))
	for i, sh := range shots {
		out = append(out, domain.Card{
			ID:    uid.New(),
			Kind:  domain.CardKindShot,
			Title: sh.Title,
			X:     float64(i) * (shotCardW + canvasCardGap),
			Y:     baseY,
			W:     shotCardW,
			H:     shotCardH,
			Z:     topZ + 1 + float64(i),
			Params: shotCardParams(domain.ShotParams{
				ShotNo:      i + 1,
				Description: sh.Description,
				Dialogue:    sh.Dialogue,
			}),
			Refs:       []string{scriptCardID},
			History:    []domain.CardVersion{},
			AutoPlaced: true,
			CreatedAt:  now,
		})
	}
	return out
}

// shotCardParams 把 ShotParams 摊成 Card.Params 的 map 形态。
//
// 走一次 JSON 往返而不是手写那六个 key：手写的那份会和 ShotParams 各自演进，
// 而 ParseShotParams 拒收未知 key——对不上的那天是在插卡时炸，且错误信息
// 指向"卡片 params 不合法"，跟真正的原因（两处 schema 漂了）毫无关系。
//
// ShotParams 全是标量字段，这一趟往返不会失败，因此不返回 error 让每个
// 调用点多一个永远走不到的分支。
func shotCardParams(sp domain.ShotParams) map[string]any {
	raw, err := json.Marshal(sp)
	if err != nil {
		panic("httpapi: marshal ShotParams: " + err.Error())
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic("httpapi: unmarshal ShotParams: " + err.Error())
	}
	return m
}

// storyboardReply 是落进对话里的那条助手消息。
//
// 明说"没有生成任何画面"：用户对这条链路的既有预期是"一说话就开始出片"，
// 不点破的话，他会盯着画布等一批永远不会出现的视频卡。
func storyboardReply(scriptTitle string, n int, modelID string) string {
	return fmt.Sprintf("已用 %s 把《%s》拆成 %d 个镜头，都在画布上，"+
		"每一镜的画面描述和台词都可以直接改。这一步只落文字，没有出图也没有出片；"+
		"改顺了之后再挑镜头去生成画面。", modelID, scriptTitle, n)
}
