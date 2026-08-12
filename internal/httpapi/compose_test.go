package httpapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter/compose"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func shotCard(id, dialogue string, shotNo int) domain.Card {
	return domain.Card{
		ID:   id,
		Kind: domain.CardKindShot,
		Params: map[string]any{
			"shot_no": shotNo, "description": "一个镜头", "dialogue": dialogue,
		},
	}
}

func videoAsset(id string, durationMS int) domain.Asset {
	ms := durationMS
	a := domain.Asset{ID: id, Type: domain.AssetTypeVideo}
	if durationMS > 0 {
		a.DurationMS = &ms
	}
	return a
}

// TestComposeSubtitlesUsesDialogueVerbatim 是字幕这一半在 httpapi 侧的验收：
// 正文必须逐字来自镜头卡的 dialogue，时间轴必须按各段真实时长累加。
//
// 「逐字」不是修辞：这条链路存在的全部理由就是台词已经在手上，不必也不该
// 去对成片跑语音转写；任何一次改写（去标点、截断、加说话人前缀）都会让
// 字幕与人声对不上，而人声是同一段文字喂给 Seedance 生成的。
func TestComposeSubtitlesUsesDialogueVerbatim(t *testing.T) {
	cards := []domain.Card{
		shotCard("c1", "你终于来了。", 1),
		shotCard("c2", "我等了很久，久到以为你不会来了。", 2),
	}
	assets := []domain.Asset{videoAsset("a1", 4000), videoAsset("a2", 5000)}

	got, err := composeSubtitles(cards, []string{"c1", "c2"}, assets)
	if err != nil {
		t.Fatalf("composeSubtitles: %v", err)
	}
	want := "1\n00:00:00,000 --> 00:00:04,000\n你终于来了。\n\n" +
		"2\n00:00:04,000 --> 00:00:09,000\n我等了很久，久到以为你不会来了。\n\n"
	if got != want {
		t.Fatalf("字幕不对：\n得到:\n%s\n期望:\n%s", got, want)
	}
}

// TestComposeSubtitlesCountsNonShotCards：用户随手拼进来的图片卡、视频卡
// 没有台词字段，它们照样占时间轴，只是不出字。
//
// 不占时间的话，排在它们后面的每一条字幕都会提前——错位量等于前面所有
// 非镜头卡的时长之和，越往后越离谱。
func TestComposeSubtitlesCountsNonShotCards(t *testing.T) {
	cards := []domain.Card{
		{ID: "img", Kind: domain.CardKindImage},
		shotCard("c1", "轮到我了", 1),
	}
	assets := []domain.Asset{
		{ID: "a-img", Type: domain.AssetTypeImage},
		videoAsset("a1", 3000),
	}

	got, err := composeSubtitles(cards, []string{"img", "c1"}, assets)
	if err != nil {
		t.Fatalf("composeSubtitles: %v", err)
	}
	// 图片段用 driver 给静态图的那个常量，两处必须同源。
	start := compose.StillSegmentMS
	if !strings.Contains(got, "00:00:02,000 --> 00:00:05,000") {
		t.Fatalf("图片段应占 %dms 并推进时间轴，实际：\n%s", start, got)
	}
	if strings.Count(got, "-->") != 1 {
		t.Fatalf("非镜头卡不该出字幕条目，实际：\n%s", got)
	}
}

// TestComposeSubtitlesRejectsUnmeasuredVideo：视频资产没有 duration_ms 时
// 整次提交要报错，而不是当 0 处理。
//
// 当 0 处理的结果是从这一段起整条字幕轨全部偏移，而一条整体偏移的字幕
// 比没有字幕更难被发现——用户会以为是模型的口型没对上。
func TestComposeSubtitlesRejectsUnmeasuredVideo(t *testing.T) {
	cards := []domain.Card{shotCard("c1", "第一句", 1), shotCard("c2", "第二句", 2)}
	assets := []domain.Asset{videoAsset("a1", 4000), videoAsset("a2", 0)}

	_, err := composeSubtitles(cards, []string{"c1", "c2"}, assets)
	if err == nil {
		t.Fatal("视频资产缺时长就该报错，否则字幕会整体错位")
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("错误应是 *domain.Error，实际 %T", err)
	}
	if de.Code != domain.CodeInvalidParam {
		t.Fatalf("错误码应为 invalid_param，实际 %s", de.Code)
	}
	if !strings.Contains(de.FieldErrors[0].Message, "a2") {
		t.Fatalf("报错要指名是哪一件产物，实际：%s", de.FieldErrors[0].Message)
	}
}

// TestComposeParamsAlwaysDeclaresBothKeys：两个开关都不开，两个键也照样要写。
//
// 目录里声明过、当前可见的参数，校验器要求提交里一个不少（validateParams 的
// !present 分支直接判「…为必填」）。省掉默认值看着干净，代价是"什么都不选的
// 合成"提交时被 400 顶回来——这正是加完开关后第一次真实合成撞上的那一条。
func TestComposeParamsAlwaysDeclaresBothKeys(t *testing.T) {
	cards := []domain.Card{shotCard("c1", "有台词", 1), shotCard("c2", "也有", 2)}
	assets := []domain.Asset{videoAsset("a1", 2000), videoAsset("a2", 2000)}

	params, err := composeParams(false, false, cards, []string{"c1", "c2"}, assets)
	if err != nil {
		t.Fatalf("composeParams: %v", err)
	}
	if got, ok := params[compose.ParamMute]; !ok || got != false {
		t.Fatalf("mute 应写成显式 false，实际：%v（present=%v）", got, ok)
	}
	if got, ok := params[compose.ParamSubtitleSRT]; !ok || got != "" {
		t.Fatalf("subtitle_srt 应写成显式空串，实际：%q（present=%v）", got, ok)
	}
	if len(params) != 2 {
		t.Fatalf("除这两个键外不该多写任何东西，实际：%v", params)
	}
}

// TestComposeParamsSkipsEmptySubtitle：一镜台词都没写时，开了字幕也不挂空轨。
//
// 一条没有任何条目的字幕轨在播放器的字幕菜单里照样占一项，用户点开发现
// 全程没字，只会以为字幕功能坏了。空串是 driver 那边"没有字幕"的判据。
func TestComposeParamsSkipsEmptySubtitle(t *testing.T) {
	cards := []domain.Card{shotCard("c1", "", 1), shotCard("c2", "  ", 2)}
	assets := []domain.Asset{videoAsset("a1", 2000), videoAsset("a2", 2000)}

	params, err := composeParams(true, true, cards, []string{"c1", "c2"}, assets)
	if err != nil {
		t.Fatalf("composeParams: %v", err)
	}
	if params[compose.ParamSubtitleSRT] != "" {
		t.Fatalf("一句台词都没有时不该挂字幕轨，实际：%v", params)
	}
	if params[compose.ParamMute] != true {
		t.Fatalf("静音开关应照常生效，实际：%v", params)
	}
}

func TestScriptOfPicks(t *testing.T) {
	cards := []domain.Card{
		{ID: "s1", Kind: domain.CardKindScript},
		{ID: "sh1", Kind: domain.CardKindShot, Refs: []string{"s1"}},
		{ID: "sh2", Kind: domain.CardKindShot, Refs: []string{"s1"}},
	}
	if got := scriptOfPicks(cards, []string{"sh1", "sh2"}); got != "s1" {
		t.Fatalf("scriptOfPicks = %q, want s1", got)
	}
	if got := scriptOfPicks(cards, []string{"s1"}); got != "" {
		t.Errorf("非镜头卡应返回空，得到 %q", got)
	}
	if got := scriptOfPicks(cards, nil); got != "" {
		t.Errorf("空 ids 应返回空，得到 %q", got)
	}
}

func TestComposedCardSlot(t *testing.T) {
	cards := []domain.Card{
		{ID: "s1", Kind: domain.CardKindScript, X: 100, Y: 0, H: 200},
		{ID: "sh1", Kind: domain.CardKindShot, Refs: []string{"s1"}, X: 100, Y: 424, H: 180},
		{ID: "sh2", Kind: domain.CardKindShot, Refs: []string{"s1"}, X: 412, Y: 424, H: 180},
	}
	x, y := composedCardSlot(cards, "s1")
	if x != 100 {
		t.Errorf("x = %v, want 100（与剧本同列）", x)
	}
	if y != 668 { // 分镜最低边 424+180=604，+64
		t.Errorf("y = %v, want 668（分镜最下之下 64）", y)
	}
}
