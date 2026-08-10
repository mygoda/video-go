package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// TestParseStoryboardShots 钉住模型回复到镜头列表的解析。
//
// 这一步要 JSON（理由见 storyboard.go 的包内注释），代价是模型有一百种把
// JSON 说歪的方式，因此每一种走样都要有确定的下场。
func TestParseStoryboardShots(t *testing.T) {
	// 模型爱把 JSON 包在 ```json 围栏里，也爱在数组前加一句客套话。
	shots, err := parseStoryboardShots(
		"好的，以下是分镜：\n```json\n"+
			`[{"title":"巷口","description":"女主撑伞回头","dialogue":"你终于来了"},`+
			`{"title":"特写","description":"雨水顺着伞骨落下","dialogue":""}]`+
			"\n```", 2)
	if err != nil {
		t.Fatalf("parseStoryboardShots failed: %v", err)
	}
	if len(shots) != 2 {
		t.Fatalf("got %d shots, want 2", len(shots))
	}
	// 台词必须留在 dialogue 里，不能并进 description：出片时台词进 prompt
	// 才有同步人声，字幕也直接取这个字段（见 domain.ShotParams）。
	if shots[0].Dialogue != "你终于来了" {
		t.Errorf("dialogue = %q, want 你终于来了", shots[0].Dialogue)
	}
	if strings.Contains(shots[0].Description, "你终于来了") {
		t.Errorf("dialogue leaked into description: %q", shots[0].Description)
	}
	// 没有台词的镜头是合法的（空镜、纯环境音），空串不该让这一镜被丢掉。
	if shots[1].Dialogue != "" {
		t.Errorf("dialogue = %q, want empty", shots[1].Dialogue)
	}

	// max_tokens 到顶时上游会在半个对象中间把话截断。整段 Unmarshal 会因为
	// 最后那半个对象作废，把前面已经完整的镜头一起丢掉。
	truncated, err := parseStoryboardShots(
		`[{"title":"一","description":"画面一","dialogue":""},`+
			`{"title":"二","description":"画面二","dialogue":""},`+
			`{"title":"三","desc`, 3)
	if err != nil {
		t.Fatalf("truncated reply failed: %v", err)
	}
	if len(truncated) != 2 {
		t.Errorf("got %d shots from a truncated reply, want the 2 complete ones", len(truncated))
	}

	// 用户要 3 个就该拿到 3 个：模型多给了丢掉多的。
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"t","description":"d","dialogue":""}`)
	}
	b.WriteString("]")
	over, err := parseStoryboardShots(b.String(), 3)
	if err != nil {
		t.Fatalf("over-long reply failed: %v", err)
	}
	if len(over) != 3 {
		t.Errorf("got %d shots, want the requested 3", len(over))
	}
	// want 越界时退回硬上限，不能让一个坏参数把画布糊满。
	capped, err := parseStoryboardShots(b.String(), 999)
	if err != nil {
		t.Fatalf("over-long reply failed: %v", err)
	}
	if len(capped) != storyboardMaxShots {
		t.Errorf("got %d shots, want the hard cap %d", len(capped), storyboardMaxShots)
	}

	// 一个都解不出来就报错，**不做任何模板兜底**——凭空造几张卡片会让人
	// 以为模型工作了。
	for _, reply := range []string{"抱歉，我无法完成这个请求。", "[]", `[{"title":"只有标题"}]`} {
		if _, err := parseStoryboardShots(reply, 3); err == nil {
			t.Errorf("reply %q was accepted; the user would get fabricated cards", reply)
		}
	}
}

// TestStoryboardPromptShotCount 钉住镜头数进了提示词、台词要求进了提示词。
//
// 镜头数写进提示词而不是"拆满再砍"：砍掉的镜头 token 已经花过，而且砍掉的
// 往往正是结尾。台词那条同理关键——模型的默认写法是把对白塞进画面描述。
func TestStoryboardPromptShotCount(t *testing.T) {
	prompt := storyboardPrompt("雨夜重逢", 7, nil)
	if !strings.Contains(prompt, "正好拆 7 个镜头") {
		t.Errorf("requested shot count never made it into the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "正好拆 3 个镜头") {
		t.Error("prompt still asks for the default count after a caller passed 7")
	}
	if !strings.Contains(prompt, "dialogue") || !strings.Contains(prompt, "不要写进 description") {
		t.Errorf("prompt does not demand dialogue be split out:\n%s", prompt)
	}
	if !strings.Contains(prompt, "雨夜重逢") {
		t.Error("the script never made it into the prompt")
	}
}

// TestResolveShotCount 钉住镜头数这个入参的三种下场：不传取默认、
// 越界报错、区间内原样放行。
//
// 超上限**报错而不是静默截断**：用户要 20 个镜头却拿回 12 个，画布上少掉的
// 那 8 个没有任何痕迹，他只会以为模型没拆完。
func TestResolveShotCount(t *testing.T) {
	n, err := resolveShotCount(nil)
	if err != nil {
		t.Fatalf("nil shot_count rejected: %v", err)
	}
	if n != storyboardDefaultShots {
		t.Errorf("default = %d, want %d", n, storyboardDefaultShots)
	}

	for _, want := range []int{1, 5, storyboardMaxShots} {
		got, err := resolveShotCount(&want)
		if err != nil {
			t.Errorf("shot_count=%d rejected: %v", want, err)
		}
		if got != want {
			t.Errorf("shot_count=%d resolved to %d", want, got)
		}
	}

	for _, bad := range []int{0, -1, 13, 20} {
		if _, err := resolveShotCount(&bad); err == nil {
			t.Errorf("shot_count=%d was accepted", bad)
		} else if de := asDomainError(err); de.Code != domain.CodeInvalidParam {
			t.Errorf("shot_count=%d gave code %s, want invalid_param", bad, de.Code)
		} else if len(de.FieldErrors) == 0 || de.FieldErrors[0].Key != "shot_count" {
			t.Errorf("shot_count=%d did not point at the offending field: %+v", bad, de.FieldErrors)
		}
	}

	// 上限那句话要说清楚上限是多少，否则用户只能挨个试。
	twenty := 20
	_, err = resolveShotCount(&twenty)
	if msg := asDomainError(err).FieldErrors[0].Message; !strings.Contains(msg, "12") ||
		!strings.Contains(msg, "20") {
		t.Errorf("over-cap message %q names neither the cap nor what was passed", msg)
	}
}

// TestShotCards 钉住镜头卡的形状：镜号从 1 起连续、refs 指回剧本卡、
// params 过得了 shot 卡自己的 schema 校验。
//
// 镜号必须来自数组下标而不是模型自己写的编号：模型偶尔从 0 开始或者跳号，
// 而镜号就是拼片顺序（见 domain.ShotParams）。
func TestShotCards(t *testing.T) {
	scriptText := "正文"
	script := domain.Card{
		ID: "card_script", Kind: domain.CardKindScript, Title: "雨夜重逢",
		Y: 10, H: 360, Z: 3, Text: &scriptText,
	}
	shots := []storyboardShot{
		{Title: "巷口", Description: "女主撑伞回头", Dialogue: "你终于来了"},
		{Title: "特写", Description: "雨水顺着伞骨落下", Dialogue: ""},
		{Title: "收尾", Description: "两人并肩走远", Dialogue: "走吧"},
	}

	cards := shotCards(shots, script.ID, []domain.Card{script}, time.Now())
	if len(cards) != 3 {
		t.Fatalf("got %d cards, want 3", len(cards))
	}
	for i, c := range cards {
		if c.Kind != domain.CardKindShot {
			t.Errorf("card %d kind = %s, want shot", i, c.Kind)
		}
		if len(c.Refs) != 1 || c.Refs[0] != script.ID {
			t.Errorf("card %d refs = %v, want [%s]", i, c.Refs, script.ID)
		}
		// 落库时会走同一个校验（见 store/mysql insertCard），这里先过一遍：
		// 未知 key 或缺失镜号在那边是一次插卡失败，且错误信息离原因很远。
		sp, err := domain.ParseShotParams(c.Params)
		if err != nil {
			t.Fatalf("card %d params rejected by the shot schema: %v", i, err)
		}
		if sp.ShotNo != i+1 {
			t.Errorf("card %d shot_no = %d, want %d", i, sp.ShotNo, i+1)
		}
		if sp.Description != shots[i].Description {
			t.Errorf("card %d description = %q, want %q", i, sp.Description, shots[i].Description)
		}
		if sp.Dialogue != shots[i].Dialogue {
			t.Errorf("card %d dialogue = %q, want %q", i, sp.Dialogue, shots[i].Dialogue)
		}
		// 六个字段恒定序列化，前端直接读，少一个就是一次 undefined
		// （见 domain.ShotParams 的注释）。
		for _, key := range []string{"shot_no", "description", "dialogue", "duration_sec", "camera", "shot_size"} {
			if _, ok := c.Params[key]; !ok {
				t.Errorf("card %d params is missing %q", i, key)
			}
		}
		// 铺在剧本卡下方，不盖住它。
		if c.Y < script.Y+script.H {
			t.Errorf("card %d at y=%v overlaps the script card (bottom %v)", i, c.Y, script.Y+script.H)
		}
	}
	// 从左到右就是镜号顺序，前端排布是 T4 的事，但不重叠是这里的事。
	for i := 1; i < len(cards); i++ {
		if cards[i].X <= cards[i-1].X {
			t.Errorf("card %d x=%v is not to the right of card %d x=%v",
				i, cards[i].X, i-1, cards[i-1].X)
		}
	}
}

// TestStoryboardSource 钉住"只能从剧本卡拆分镜"这条前置校验。
//
// 三种失败分别报清楚：不在这张画布上、不是剧本卡、还没有正文。
// 回一句"参数错误"等于让用户挨个点卡片试。
func TestStoryboardSource(t *testing.T) {
	text := "正文"
	empty := "   "
	cards := []domain.Card{
		{ID: "s1", Kind: domain.CardKindScript, Text: &text},
		{ID: "s2", Kind: domain.CardKindScript, Text: &empty},
		{ID: "v1", Kind: domain.CardKindVideo},
	}

	got, err := storyboardSource(cards, "s1")
	if err != nil {
		t.Fatalf("a valid script card was rejected: %v", err)
	}
	if got.ID != "s1" {
		t.Errorf("picked %q, want s1", got.ID)
	}

	for _, tc := range []struct{ name, id string }{
		{"不在本画布上", "nope"},
		{"不是剧本卡", "v1"},
		{"剧本卡还没有正文", "s2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := storyboardSource(cards, tc.id)
			if err == nil {
				t.Fatalf("card %q was accepted", tc.id)
			}
			de := asDomainError(err)
			if de.Code != domain.CodeInvalidParam {
				t.Errorf("code = %s, want invalid_param", de.Code)
			}
			if len(de.FieldErrors) == 0 || de.FieldErrors[0].Key != "card_id" {
				t.Errorf("error does not point at card_id: %+v", de.FieldErrors)
			}
		})
	}
}
