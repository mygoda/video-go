// 分镜拆解：把一份剧本交给 chat 模型拆成若干镜头。
//
// # 本文件当前没有被任何 HTTP 入口调用，这是有意的
//
// 画布对话这一步现在只产出剧本卡（见 script.go）。拆分镜是紧随其后的
// 独立一步，由 DEM-84 接上入口。下面这套提示词与解析（尤其是写死 3 个
// 镜头的那段成本论证）是上一版实测调出来的，删掉再照着注释重写一遍
// 只会把同样的坑再踩一次，因此原样留在这里等它的入口。
//
// 与剧本那一步同理，拆解本身同步：产物是文字，chat 族一次 HTTP 往返就有
// 结果，包装成异步任务链路只是让用户多盯一会儿转圈（见 chatcall.go）。
//
// **拆解不出片。** 上一版在这里为每个镜头立刻派一条真视频任务，用户还没
// 看见任何东西钱就花出去了——那条派发链路已经整个删掉，出片是用户看过、
// 改过镜头之后另一步的事。
package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

const (
	// storyboardMaxShots 是一次拆解最多落多少张卡片。上界不是性能考虑，
	// 是防止模型把一句话拆成四十个镜头把画布糊满。
	storyboardMaxShots = 12

	// storyboardDefaultShots 是提示词里要求模型拆出的镜头数。
	//
	// **它是一个成本闸门，不是审美选择。** 每个镜头最终都要真调一次视频模型：
	// 一段 4 秒 480p 约 80 秒、要花真金白银，12 段就是十几分钟加十几倍的钱，
	// 而分镜的价值在"看到这个故事被切成了几个画面"，第 4 段之后的边际信息
	// 迅速趋近于零。取 3 段：足够呈现起承转合，一轮总时长仍在用户愿意等的
	// 量级内。要更多镜头就再说一句话继续拆，那是用户主动选择多花的钱。
	//
	// 写进提示词而不是"拆 12 个再砍到 3 个"：砍掉的那 9 个镜头模型已经算过、
	// token 已经花过，而且被砍掉的往往正是结尾，留下的三段讲不完一个故事。
	storyboardDefaultShots = 3
)

// storyboardShot 是模型返回的一个镜头。字段名就是喂给模型的 JSON 契约，
// 改这里必须同时改 storyboardPrompt 里的示例。
type storyboardShot struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// storyboardPrompt 拼出发给模型的那段话。
//
// 要求模型只回 JSON 数组：拿自然语言回来还得再写一个中文分镜格式的解析器，
// 而那个解析器会在模型换一种排版时立刻失效。JSON 至少是可判定的——
// 解析失败就是解析失败，不会似是而非地拆出半截。
//
// （剧本那一步反过来要纯文本，理由见 script.go 的包内注释：一整篇带换行的
// 散文塞进 JSON 字符串，模型漏转义一次就整篇作废。）
func storyboardPrompt(script string, refs []domain.Card) string {
	var b strings.Builder
	b.WriteString("你是一位分镜师。把下面这段内容拆成若干个连续的镜头。\n\n")
	b.WriteString("要求：\n")
	b.WriteString("1. 只输出一个 JSON 数组，不要输出任何解释、前后缀或代码块标记。\n")
	b.WriteString("2. 数组每一项形如 {\"title\": \"镜头标题\", \"description\": \"这个镜头的画面描述\"}。\n")
	b.WriteString("3. title 控制在 12 个字以内，description 是可以直接用于文生视频的画面描述，")
	b.WriteString("包含主体、动作、环境、镜头语言。\n")
	b.WriteString(fmt.Sprintf("4. 正好拆 %d 个镜头，不多不少；如果内容很长，就挑最关键的 %d 个画面。\n",
		storyboardDefaultShots, storyboardDefaultShots))

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
