package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store/mysql"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 本文件守的是「管理端的重试只重试 executor 执行的任务」这条契约。
//
// 画布 chat / storyboard / refine 三条链路为了让积分流水有可对账的 task_id
// 也在 tasks 里落行（见 chatbill.go 的包注释），但它们是在 HTTP handler 里同步
// 跑完的，产物落点是 canvas_cards。把这种行打回 queued，worker 会照着 prompt
// 真打一次上游、真花一份钱，而产物只能转存成一个孤儿 text 资产——用户画布上
// 那张卡纹丝不动。
//
// 断言落在**三件事**上：拒绝的状态码、库里那行的状态没被动过、以及上游调用
// 次数没涨。只看状态码证明不了任何事：先 Requeue 再回错误，状态码一模一样，
// 而任务已经回到队列里等着被重打了。
//
// 复用 chatbill_test.go 的 billFixture（同一个 AIGC_TEST_MYSQL_DSN 开关），
// 因为要拒绝的那种行只有真跑一次画布链路才造得出来——手工插一行 step='script'
// 只能证明闸认得这个值，证不了那三条链路真的会落上它。

// retryTask 直接打 handleAdminRetryTask。
//
// 不经路由：这一层要验的是 handler 的判定，而不是 mux 的注册。taskId 用
// SetPathValue 塞进去，与 net/http 的路由模式取值走的是同一条路。
func retryTask(t *testing.T, srv *server, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/tasks/"+taskID+"/retry", nil)
	req.SetPathValue("taskId", taskID)
	rec := httptest.NewRecorder()
	srv.handleAdminRetryTask(rec, req)
	return rec
}

func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) domain.ErrorCode {
	t.Helper()
	var env domain.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析错误响应失败：%v（body=%s）", err, rec.Body.String())
	}
	return env.Error.Code
}

// TestAdminRetryRefusesCanvasChatTask 是本票的主验收：一条写剧本落下的任务
// 打重试要被顶回来，且任务状态与上游调用次数都不许变。
func TestAdminRetryRefusesCanvasChatTask(t *testing.T) {
	f := newBillFixture(t, 100)
	ctx := context.Background()

	_, _, bill, err := f.srv.generateScript(ctx, f.call(), "写一个雨夜重逢的故事", nil)
	if err != nil {
		t.Fatalf("写剧本失败：%v", err)
	}
	bill.charge(ctx)

	entries := f.ledger(t)
	if len(entries) == 0 {
		t.Fatal("没有流水，拿不到要重试的那条任务")
	}
	taskID := entries[0].TaskID

	tasks := mysql.NewTaskRepo(f.db)
	before, err := tasks.Get(ctx, taskID)
	requireNoTestErr(t, err, "reload task")
	if before.Step != "script" {
		t.Fatalf("task step = %q, want \"script\"：来源没落库，闸判什么都是空的", before.Step)
	}
	if before.Status != domain.TaskStatusSucceeded {
		t.Fatalf("前置条件不成立：task status = %q, want succeeded", before.Status)
	}
	callsBefore := f.driver.calls

	rec := retryTask(t, f.srv, taskID)

	if rec.Code != http.StatusConflict {
		t.Errorf("重试返回 %d, want %d：画布对话类任务被放进了队列", rec.Code, http.StatusConflict)
	}
	if code := decodeErrorCode(t, rec); code != domain.CodeConflict {
		t.Errorf("错误码 = %q, want %q", code, domain.CodeConflict)
	}

	after, err := tasks.Get(ctx, taskID)
	requireNoTestErr(t, err, "reload task after retry")
	if after.Status != domain.TaskStatusSucceeded {
		t.Errorf("重试后 task status = %q, want succeeded：这一行已经回到队列，worker 会重打一次上游",
			after.Status)
	}
	if f.driver.calls != callsBefore {
		t.Errorf("上游调用次数 %d → %d：重试真的把钱又花了一次", callsBefore, f.driver.calls)
	}
	if now := f.ledger(t); len(now) != len(entries) {
		t.Errorf("流水条数 %d → %d：被拒绝的重试不该动账", len(entries), len(now))
	}
}

// TestAdminRetryRefusesStoryboardAndRefineTasks 把另外两条链路一起钉住。
//
// 三条链路共用 holdChat 落行，但 step 是各自填的（script.go / storyboard.go），
// 漏填一条就等于那条链路照旧可以被重试，而这种漏在主用例里看不出来。
func TestAdminRetryRefusesStoryboardAndRefineTasks(t *testing.T) {
	text := "第一场 巷口，夜。老张撑着伞站在路灯下。"
	card := domain.Card{ID: uid.New(), Kind: domain.CardKindScript, Title: "旧标题", Text: &text}

	cases := []struct {
		step  string
		reply string
		run   func(f *billFixture, ctx context.Context) (*chatBill, error)
	}{
		{"storyboard", storyboardTestReply, func(f *billFixture, ctx context.Context) (*chatBill, error) {
			_, _, _, bill, err := f.srv.generateStoryboard(ctx, f.call(), text, 3, nil, nil)
			return bill, err
		}},
		{"refine", "新标题\n第一场 巷口，夜。老王撑着伞站在路灯下。", func(f *billFixture, ctx context.Context) (*chatBill, error) {
			_, _, bill, err := f.srv.refineScript(ctx, f.call(), card, "把主角换成女性")
			return bill, err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.step, func(t *testing.T) {
			f := newBillFixture(t, 100)
			ctx := context.Background()
			// 每一步的产物形状不同：分镜要能解析出镜头的 JSON，改写要"第一行
			// 标题 + 正文"。回复不对会在 parse 那一步就退款失败，闸都轮不到判。
			f.driver.reply = tc.reply

			bill, err := tc.run(f, ctx)
			if err != nil {
				t.Fatalf("%s 失败：%v", tc.step, err)
			}
			bill.charge(ctx)

			entries := f.ledger(t)
			if len(entries) == 0 {
				t.Fatal("没有流水，拿不到要重试的那条任务")
			}
			taskID := entries[0].TaskID

			tasks := mysql.NewTaskRepo(f.db)
			before, err := tasks.Get(ctx, taskID)
			requireNoTestErr(t, err, "reload task")
			if before.Step != tc.step {
				t.Fatalf("task step = %q, want %q", before.Step, tc.step)
			}

			if rec := retryTask(t, f.srv, taskID); rec.Code != http.StatusConflict {
				t.Errorf("重试返回 %d, want %d", rec.Code, http.StatusConflict)
			}
			after, err := tasks.Get(ctx, taskID)
			requireNoTestErr(t, err, "reload task after retry")
			if after.Status != before.Status {
				t.Errorf("重试后 task status = %q, want %q", after.Status, before.Status)
			}
		})
	}
}

// storyboardTestReply 是一份能被 parseStoryboardShots 解出 3 个镜头的回复。
const storyboardTestReply = `[
  {"shot_no": 1, "description": "巷口全景，雨。", "dialogue": ""},
  {"shot_no": 2, "description": "老张的特写。", "dialogue": "你还是来了。"},
  {"shot_no": 3, "description": "两人并肩走远。", "dialogue": ""}
]`

// TestAdminRetryRequeuesExecutorTask 是这道闸的反面：普通任务没被误伤。
//
// 特意给它带上 canvas_id 与 card_id——画布内发起、单卡重跑的图片任务本来就
// 带着这两个字段。闸如果按"有画布坐标 + 文本模态"这类特征推断来源，这条
// 用例就会红：它是把"来源"写下来而不是猜出来的那个决定的守门人。
func TestAdminRetryRequeuesExecutorTask(t *testing.T) {
	f := newBillFixture(t, 100)
	ctx := context.Background()

	imageModelID := "dem115-image-" + uid.Token(8)
	_, err := mysql.NewModelRepo(f.db).Upsert(ctx, domain.ModelConfig{
		ID: imageModelID, ProviderID: f.provID, UpstreamModel: "upstream-" + imageModelID,
		Family: domain.FamilyImages, Enabled: true, Visibility: domain.VisibilityPublic,
		Capability: map[string]any{
			"id": imageModelID, "name": "DEM-115 Test Image Model", "vendor": "test",
			"modality": "image", "enabled": true, "order": 1,
			"inputs": []any{}, "params": []any{},
			"pricing": map[string]any{"currency": "credit", "base": 1, "rounding": "ceil"},
			"eta":     map[string]any{"p50_seconds": 5, "p90_seconds": 20},
			"limits":  map[string]any{"max_concurrent_per_user": 0, "prompt_max_length": 0},
		},
	})
	requireNoTestErr(t, err, "create image model")

	canvasID, cardID := uid.New(), uid.New()
	tasks := mysql.NewTaskRepo(f.db)
	task, err := tasks.Create(ctx, domain.Task{
		UserID: f.userID, ModelID: imageModelID, ProviderID: f.provID,
		Status: domain.TaskStatusFailed, Prompt: "一只在雨夜奔跑的猫",
		CanvasID: &canvasID, CardID: &cardID,
	})
	requireNoTestErr(t, err, "create image task")
	if task.Step != "" {
		t.Fatalf("普通任务不该带 step，实际 %q", task.Step)
	}

	rec := retryTask(t, f.srv, task.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("重试返回 %d, want 200（body=%s）：普通任务被闸误伤了", rec.Code, rec.Body.String())
	}

	after, err := tasks.Get(ctx, task.ID)
	requireNoTestErr(t, err, "reload image task")
	if after.Status != domain.TaskStatusQueued {
		t.Errorf("重试后 task status = %q, want queued", after.Status)
	}
}
