package httpapi

import (
	"strings"
	"testing"
)

// TestParseScriptReply 钉住剧本回复的切分：第一行是标题，其余是正文。
//
// 这一步不用 JSON（理由见 script.go 的包内注释：一整篇带换行的散文塞进
// JSON 字符串，模型漏转义一次就整篇作废），代价是标题得靠一条约定切出来，
// 因此这条约定的每种走样都要有确定的下场。
func TestParseScriptReply(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		title string
		text  string
	}{
		{
			name:  "第一行标题，其余正文",
			reply: "雨夜重逢\n\n第一场 巷口\n女主撑伞回头。",
			title: "雨夜重逢",
			text:  "第一场 巷口\n女主撑伞回头。",
		},
		{
			// 模型爱给标题加 Markdown 井号或「标题：」前缀，留着的话
			// 卡片标题栏会显示成「## 雨夜重逢」。
			name:  "剥掉标题上的装饰",
			reply: "## 《雨夜重逢》\n正文",
			title: "雨夜重逢",
			text:  "正文",
		},
		{
			name:  "标题行写成「标题：」",
			reply: "标题：雨夜重逢\n正文",
			title: "雨夜重逢",
			text:  "正文",
		},
		{
			// 只有一行时不硬拆：那一行既是标题也是正文。把正文清空
			// 只会让卡片看着像生成失败了，而它其实没有。
			name:  "只有一行时正文不清空",
			reply: "一个关于雨夜重逢的短故事",
			title: "一个关于雨夜重逢的短故事",
			text:  "一个关于雨夜重逢的短故事",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseScriptReply(tc.reply)
			if err != nil {
				t.Fatalf("parseScriptReply(%q) failed: %v", tc.reply, err)
			}
			if got.Title != tc.title {
				t.Errorf("title = %q, want %q", got.Title, tc.title)
			}
			if got.Text != tc.text {
				t.Errorf("text = %q, want %q", got.Text, tc.text)
			}
		})
	}

	// 超长标题截断，否则卡片标题栏会被一整段撑爆。
	long, err := parseScriptReply(strings.Repeat("很", 80) + "\n正文")
	if err != nil {
		t.Fatalf("long title failed: %v", err)
	}
	if n := len([]rune(long.Title)); n > scriptTitleMaxRunes+1 {
		t.Errorf("title kept %d runes, want it truncated near %d", n, scriptTitleMaxRunes)
	}

	// 空回复报错，不做任何模板兜底——凭空造一份剧本会让人以为模型工作了。
	if _, err := parseScriptReply("   \n\n  "); err == nil {
		t.Error("an empty reply was accepted; the user would get a blank script card")
	}
}

// TestScriptPromptForbidsShots 钉住提示词明确禁止拆分镜。
//
// 模型见到"短剧"两个字的默认反应就是直接给一张分镜表，而分镜得等用户
// 先把剧本改顺了再拆——这一步吐出分镜就等于把 T3 的停顿点又吞掉了。
func TestScriptPromptForbidsShots(t *testing.T) {
	prompt := scriptPrompt("雨夜重逢", nil)
	if !strings.Contains(prompt, "不要拆分镜") {
		t.Errorf("script prompt does not forbid storyboarding:\n%s", prompt)
	}
	if !strings.Contains(prompt, "雨夜重逢") {
		t.Error("the user's idea never made it into the prompt")
	}
}
