package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store/mysql"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 本文件守的是「画布那三条链路落进 tasks.prompt 的东西不含系统提示词」这条
// 契约。它与 chatbill_test.go 守的是同一批 task 行的两面：那边验这一行**在**
// （钱有对账对象），这边验这一行**只装用户自己的话**。
//
// 断言落在**从库里回读的那一列**上，而不是 chatCall 的字段：GET /api/tasks
// 回给用户的正是这一列（handlers_tasks 直接序列化 domain.Task），中间还隔着
// 落库与回读，只有查库才验得到用户真正拿得到的那个值。
//
// 复用 chatbill_test.go 的 billFixture：同一套假 driver 与真库开关，
// 没设 AIGC_TEST_MYSQL_DSN 时一并 t.Skip。

// systemPromptMarkers 是三段系统提示词里各自最有辨识度的句子。
//
// 逐句列而不是比对整段：提示词以后还会改措辞，钉整段会让这个测试在任何一次
// 文案调整后失败；而这几句一旦出现在对外响应里，泄的就是同一个东西。
var systemPromptMarkers = []string{
	"你是一位短剧编剧",
	"你是一位分镜师",
	"只改用户要求改的部分",
	"不要拆分镜",
	"只输出一个 JSON 数组",
}

func assertNoSystemPrompt(t *testing.T, step, stored string) {
	t.Helper()
	for _, marker := range systemPromptMarkers {
		if strings.Contains(stored, marker) {
			t.Errorf("%s 落库的 prompt 里出现了系统提示词片段 %q；用户拉 GET /api/tasks 就能看到：%q",
				step, marker, stored)
		}
	}
}

// taskPrompt 回读一条任务落在库里的 prompt 列。
func (f *billFixture) taskPrompt(t *testing.T, taskID string) string {
	t.Helper()
	task, err := mysql.NewTaskRepo(f.db).Get(context.Background(), taskID)
	requireNoTestErr(t, err, "reload task")
	return task.Prompt
}

// TestScriptTaskPromptStoresUserIdeaOnly：写剧本落的是用户发的那句话本身。
func TestScriptTaskPromptStoresUserIdeaOnly(t *testing.T) {
	f := newBillFixture(t, 100)
	ctx := context.Background()
	const idea = "写一个雨夜重逢的故事"

	_, _, bill, err := f.srv.generateScript(ctx, f.call(), idea, nil)
	if err != nil {
		t.Fatalf("写剧本失败：%v", err)
	}
	bill.charge(ctx)

	got := f.taskPrompt(t, bill.taskID)
	if got != idea {
		t.Errorf("落库 prompt = %q, want %q", got, idea)
	}
	assertNoSystemPrompt(t, "script", got)
}

// TestRefineTaskPromptStoresInstructionOnly：优化落的是用户写的修改要求。
//
// 顺带钉住"原剧本正文没有跟着落进来"：它是提示词的一部分（refinePrompt 会把
// 整篇发上去），却不是用户这一次说的话。
func TestRefineTaskPromptStoresInstructionOnly(t *testing.T) {
	f := newBillFixture(t, 100)
	ctx := context.Background()

	text := "第一场 巷口，夜。老张撑着伞站在路灯下。"
	card := domain.Card{ID: uid.New(), Kind: domain.CardKindScript, Title: "雨夜重逢", Text: &text}
	const instruction = "把主角换成女性"

	_, _, bill, err := f.srv.refineScript(ctx, f.call(), card, instruction)
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	bill.charge(ctx)

	got := f.taskPrompt(t, bill.taskID)
	if got != instruction {
		t.Errorf("落库 prompt = %q, want %q", got, instruction)
	}
	assertNoSystemPrompt(t, "refine", got)
}

// TestStoryboardTaskPromptStoresScriptExcerpt：拆分镜没有用户输入，落的是被拆
// 那张剧本卡的正文摘要，并且长剧本要截断。
func TestStoryboardTaskPromptStoresScriptExcerpt(t *testing.T) {
	f := newBillFixture(t, 100)
	f.driver.reply = `[{"title":"巷口","description":"雨夜巷口，老张撑伞站在路灯下","dialogue":""}]`
	ctx := context.Background()

	const script = "第一场 巷口，夜。老张撑着伞站在路灯下。"
	_, _, bill, err := f.srv.generateStoryboard(ctx, f.call(), script, 1, nil, nil)
	if err != nil {
		t.Fatalf("拆分镜失败：%v", err)
	}
	bill.charge(ctx)

	got := f.taskPrompt(t, bill.taskID)
	if got != script {
		t.Errorf("落库 prompt = %q, want %q（短于上限时摘要就是原文）", got, script)
	}
	assertNoSystemPrompt(t, "storyboard", got)

	// 整篇剧本远长于 storyboardInputRunes：这一列只用来认人，不该装下一份剧本。
	long := strings.Repeat("镜", storyboardInputRunes+50)
	_, _, bill2, err := f.srv.generateStoryboard(ctx, f.call(), long, 1, nil, nil)
	if err != nil {
		t.Fatalf("拆长剧本失败：%v", err)
	}
	bill2.charge(ctx)

	// 截断后多出来的那一个 rune 是 truncateRunes 补的省略号。
	if n := len([]rune(f.taskPrompt(t, bill2.taskID))); n > storyboardInputRunes+1 {
		t.Errorf("摘要没截断：%d runes, want <= %d", n, storyboardInputRunes+1)
	}
}
