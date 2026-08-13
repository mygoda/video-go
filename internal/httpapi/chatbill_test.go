package httpapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store/mysql"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 本文件守的是「画布那三条同步 chat 链路要真的扣钱」这条契约。
//
// 断言全部落在 **credit_ledger 的行**上，不是"handler 调没调 Ledger"：
// 这三条链路此前一分不扣，正是因为没人调它；而"调了"与"钱确实动了"之间
// 还隔着 hold/charge 配对、冻结释放、余额物化三件事，只有查库能同时验到。
//
// 上游用一个受控的假 driver，因此每条用例都能精确回答一个此前没法回答的
// 问题：**余额不够时上游到底有没有被调用**（billDriver.calls）。
//
// 复用 task_inputs_test.go 的 AIGC_TEST_MYSQL_DSN 开关与 requireNoTestErr：
// 没设它时 t.Skip，`go test ./...` 在一台没有 MySQL 的机器上仍要跑得通。

// chatBillPrice 是测试模型声明的一口价。
//
// 取 7 这种不常见的数：断言看到 7 就知道钱是从 pricing.base 算出来的，
// 而不是撞上了某个默认值 1 或 0。
const chatBillPrice = 7

// billDriver 是一个受控的 chat 上游。
//
// calls 是本文件最重要的那个字段：「余额不足时上游根本不会被调用」这条
// 验收，只有数调用次数才证得了，看错误码只能证明"最后回了 402"。
type billDriver struct {
	calls int
	reply string
	err   error
}

func (d *billDriver) Name() string                  { return string(domain.FamilyChat) }
func (d *billDriver) Family() domain.ProtocolFamily { return domain.FamilyChat }

func (d *billDriver) Invoke(context.Context, adapter.SubmitInput) (adapter.SubmitResult, error) {
	d.calls++
	if d.err != nil {
		return adapter.SubmitResult{}, d.err
	}
	// 形状照 openaicompat.chatArtifacts：base64 + text/plain。
	return adapter.SubmitResult{Inline: []adapter.ArtifactRef{{
		Kind:   adapter.KindBase64,
		Type:   domain.AssetTypeText,
		MIME:   "text/plain",
		Base64: base64.StdEncoding.EncodeToString([]byte(d.reply)),
	}}}, nil
}

type billFixture struct {
	db      *sql.DB
	srv     *server
	driver  *billDriver
	userID  string
	modelID string
	provID  string
	credits int
}

// newBillFixture 造一套连着真库的最小计费环境：一个有 credits 分的用户、
// 一个一口价 chatBillPrice 的 chat 模型，以及一个只挂着假 driver 的注册表。
func newBillFixture(t *testing.T, credits int) *billFixture {
	t.Helper()
	dsn := os.Getenv(assetInputsDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; skipping MySQL integration test", assetInputsDSNEnv)
	}
	db, err := mysql.Open(dsn)
	if err != nil {
		t.Fatalf("open %s: %v", assetInputsDSNEnv, err)
	}
	ctx := context.Background()
	f := &billFixture{db: db, credits: credits}

	// 凭证只在本用例的进程环境里存在：chatOnce 会检查它非空，
	// 而这一步失败是免费的，不该把用例卡在"环境变量没设"上。
	const credRef = "AIGC_TEST_CHATBILL_CREDENTIAL"
	t.Setenv(credRef, "dem111-test-credential")

	users := mysql.NewUserRepo(db)
	u, err := users.Create(ctx, domain.User{
		Username: "dem111_user_" + uid.Token(8), PasswordHash: "argon2id$test",
		Credits: credits, StorageQuota: 1 << 30,
	})
	requireNoTestErr(t, err, "create user")
	f.userID = u.ID

	prov, err := mysql.NewProviderRepo(db).Upsert(ctx, domain.Provider{
		Name: "dem111-provider-" + uid.Token(8), BaseURL: "https://example.invalid",
		CredentialRef: credRef, Enabled: true,
	})
	requireNoTestErr(t, err, "create provider")
	f.provID = prov.ID

	modelID := "dem111-chat-" + uid.Token(8)
	m, err := mysql.NewModelRepo(db).Upsert(ctx, domain.ModelConfig{
		ID: modelID, ProviderID: prov.ID, UpstreamModel: "upstream-" + modelID,
		Family: domain.FamilyChat, Enabled: true,
		Visibility: domain.VisibilityPublic,
		Capability: chatBillCapability(modelID),
	})
	requireNoTestErr(t, err, "create model")
	f.modelID = m.ID

	reg := adapter.NewRegistry().(adapter.Mutable)
	f.driver = &billDriver{reply: "雨夜重逢\n第一场 巷口，夜。老张撑着伞站在路灯下。"}
	reg.MustRegister(f.driver)

	eval := capability.NewEvaluator()
	f.srv = &server{deps: Deps{
		// 指定 StoryboardModelID 让默认选模型这条路稳定落到本用例的模型上，
		// 不受测试库里其他 chat 模型的 display_order 影响。
		Config:    config.Config{StoryboardModelID: modelID},
		Users:     users,
		Models:    mysql.NewModelRepo(db),
		Providers: mysql.NewProviderRepo(db),
		Tasks:     mysql.NewTaskRepo(db),
		Evaluator: eval,
		Pricer:    capability.NewPricer(eval),
		Drivers:   reg,
		Ledger:    mysql.NewLedger(db),
	}}

	t.Cleanup(func() { f.drop(t); _ = db.Close() })
	return f
}

// chatBillCapability 是一份最小的 chat 能力声明，形状照 000016 播的文本模型。
func chatBillCapability(modelID string) map[string]any {
	return map[string]any{
		"id": modelID, "name": "DEM-111 Test Chat Model", "vendor": "test",
		"modality": "text", "enabled": true, "order": 1,
		"inputs": []any{},
		"params": []any{map[string]any{
			"key": "max_tokens", "label": "最长输出", "control": "stepper",
			"group": "advanced", "order": 10,
			"default": 4096, "min": 256, "max": 8192, "step": 256, "unit": "token",
		}},
		"pricing": map[string]any{"currency": "credit", "base": chatBillPrice, "rounding": "ceil"},
		"eta":     map[string]any{"p50_seconds": 5, "p90_seconds": 20},
		"limits":  map[string]any{"max_concurrent_per_user": 0, "prompt_max_length": 0},
	}
}

// drop 按外键顺序清掉固件造出来的数据：流水指向任务，任务指向用户与模型。
func (f *billFixture) drop(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM credit_ledger WHERE user_id = ?`,
		`DELETE FROM task_events WHERE user_id = ?`,
		`DELETE FROM tasks WHERE user_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := f.db.ExecContext(ctx, q, f.userID); err != nil {
			t.Logf("cleanup %q: %v", q, err)
		}
	}
	for _, q := range []string{
		`DELETE FROM models WHERE provider_id = ?`,
		`DELETE FROM providers WHERE id = ?`,
	} {
		if _, err := f.db.ExecContext(ctx, q, f.provID); err != nil {
			t.Logf("cleanup %q: %v", q, err)
		}
	}
}

// ledgerEntry 是从库里读回来的一条流水，只留断言要看的四列。
type ledgerEntry struct {
	Type   string
	Amount int
	TaskID string
	Reason string
}

func (f *billFixture) ledger(t *testing.T) []ledgerEntry {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT type, amount, COALESCE(task_id, ''), reason
		   FROM credit_ledger WHERE user_id = ? ORDER BY created_at, id`, f.userID)
	requireNoTestErr(t, err, "query credit_ledger")
	defer rows.Close()

	var out []ledgerEntry
	for rows.Next() {
		var e ledgerEntry
		requireNoTestErr(t, rows.Scan(&e.Type, &e.Amount, &e.TaskID, &e.Reason), "scan ledger row")
		out = append(out, e)
	}
	requireNoTestErr(t, rows.Err(), "iterate ledger rows")
	return out
}

func (f *billFixture) balance(t *testing.T) domain.Balance {
	t.Helper()
	bal, err := f.srv.deps.Ledger.Balance(context.Background(), f.userID)
	requireNoTestErr(t, err, "read balance")
	return bal
}

func (f *billFixture) call() chatCall {
	return chatCall{userID: f.userID}
}

// TestChatBillHoldsThenCharges 是本票的主验收：一次成功的写剧本必须冻一笔、
// 结一笔，余额真的少掉 chatBillPrice 分。
//
// 分两段断言（charge 之前、之后）而不是只看最终余额：冻结释放漏掉时最终
// 可用余额同样是对的，错的是 credits_held 一直挂着——那笔钱用户再也花不出去。
func TestChatBillHoldsThenCharges(t *testing.T) {
	f := newBillFixture(t, 100)
	ctx := context.Background()

	draft, trace, bill, err := f.srv.generateScript(ctx, f.call(), "写一个雨夜重逢的故事", nil)
	if err != nil {
		t.Fatalf("写剧本失败：%v", err)
	}
	if bill == nil {
		t.Fatal("成功返回却没有 bill：这次调用烧了 token 却没有可结算的对象")
	}
	if f.driver.calls != 1 {
		t.Errorf("上游调用次数 = %d, want 1", f.driver.calls)
	}
	if draft.Title != "雨夜重逢" || trace.ModelID != f.modelID {
		t.Errorf("draft/trace 不对：title=%q model=%q", draft.Title, trace.ModelID)
	}

	// 冻结已经发生，但还没结算：可用余额已经少了，钱挂在 credits_held 上。
	if bal := f.balance(t); bal.Available != 100-chatBillPrice || bal.Held != chatBillPrice {
		t.Fatalf("冻结后余额 = %+v, want available=%d held=%d",
			bal, 100-chatBillPrice, chatBillPrice)
	}

	bill.charge(ctx)

	bal := f.balance(t)
	if bal.Available != 100-chatBillPrice {
		t.Errorf("结算后可用余额 = %d, want %d", bal.Available, 100-chatBillPrice)
	}
	if bal.Held != 0 {
		t.Errorf("结算后冻结额 = %d, want 0：冻结没释放，这笔钱谁也用不了", bal.Held)
	}

	entries := f.ledger(t)
	t.Logf("credit_ledger for user %s: %+v", f.userID, entries)
	if len(entries) != 2 {
		t.Fatalf("流水条数 = %d, want 2（hold + charge）：%+v", len(entries), entries)
	}
	if entries[0].Type != "hold" || entries[0].Amount != -chatBillPrice {
		t.Errorf("第一条流水 = %+v, want hold/-%d", entries[0], chatBillPrice)
	}
	// charge 的 amount 是"退回多冻结的部分"，一口价下实扣等于冻结，因此为 0。
	if entries[1].Type != "charge" || entries[1].Amount != 0 {
		t.Errorf("第二条流水 = %+v, want charge/0", entries[1])
	}
	if entries[0].TaskID == "" || entries[0].TaskID != entries[1].TaskID {
		t.Fatalf("hold 与 charge 没配在同一个 task 上：%+v", entries)
	}

	// 任务行是这笔钱的对账对象：状态、实扣额都要落下来，
	// ProfilePage 的账单文案正是靠 task_id 回查模态与模型名的。
	task, err := mysql.NewTaskRepo(f.db).Get(ctx, entries[0].TaskID)
	requireNoTestErr(t, err, "reload task")
	if task.Status != domain.TaskStatusSucceeded {
		t.Errorf("task status = %q, want succeeded", task.Status)
	}
	if task.ActualCost == nil || *task.ActualCost != chatBillPrice {
		t.Errorf("task actual_cost = %v, want %d", task.ActualCost, chatBillPrice)
	}
}

// TestChatBillRejectsInsufficientCreditBeforeUpstream 钉住这张票的第二条验收：
// 余额不够时**上游一次都没被调用**。
//
// driver.calls 是这条断言的全部证据。只看错误码证明不了任何事——先打上游
// 再发现钱不够、然后回 402，错误码一模一样，而平台已经付过那次 token 了。
func TestChatBillRejectsInsufficientCreditBeforeUpstream(t *testing.T) {
	f := newBillFixture(t, chatBillPrice-1)
	ctx := context.Background()

	_, _, bill, err := f.srv.generateScript(ctx, f.call(), "写一个雨夜重逢的故事", nil)
	if err == nil {
		t.Fatal("余额不足却写出了剧本")
	}
	if bill != nil {
		t.Error("失败却返回了 bill：调用方会以为有一笔钱要结")
	}
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInsufficientCredit {
		t.Fatalf("错误 = %v, want %s", err, domain.CodeInsufficientCredit)
	}
	if f.driver.calls != 0 {
		t.Errorf("上游被调用了 %d 次：闸建在了调用之后，钱照烧", f.driver.calls)
	}
	if entries := f.ledger(t); len(entries) != 0 {
		t.Errorf("没扣成却留下了流水：%+v", entries)
	}
	if bal := f.balance(t); bal.Available != chatBillPrice-1 || bal.Held != 0 {
		t.Errorf("余额被动过了：%+v", bal)
	}
}

// TestChatBillRefundsUpstreamFailure 钉住第三条验收：上游失败全额退。
//
// 顺带钉住退款理由的措辞。前端账单按冒号切最后一段取错误码
// （ProfilePage.describeEntry），这里与 executor.fail 分叉就会显示成泛化文案。
func TestChatBillRefundsUpstreamFailure(t *testing.T) {
	f := newBillFixture(t, 100)
	f.driver.err = &domain.Error{
		Code: domain.CodeUpstreamRateLimited, Message: "上游限流", Retryable: true,
	}
	ctx := context.Background()

	_, _, bill, err := f.srv.generateScript(ctx, f.call(), "写一个雨夜重逢的故事", nil)
	if err == nil {
		t.Fatal("上游失败却返回了成功")
	}
	if bill != nil {
		t.Error("失败却返回了 bill：这笔冻结已经在内部退掉了，再退一次会撞幂等闸")
	}
	if f.driver.calls != 1 {
		t.Errorf("上游调用次数 = %d, want 1", f.driver.calls)
	}

	bal := f.balance(t)
	if bal.Available != 100 || bal.Held != 0 {
		t.Errorf("退款后余额 = %+v, want available=100 held=0：用户为一次失败付了钱", bal)
	}

	entries := f.ledger(t)
	t.Logf("credit_ledger for user %s: %+v", f.userID, entries)
	if len(entries) != 2 {
		t.Fatalf("流水条数 = %d, want 2（hold + refund）：%+v", len(entries), entries)
	}
	if entries[1].Type != "refund" || entries[1].Amount != chatBillPrice {
		t.Errorf("退款流水 = %+v, want refund/%d", entries[1], chatBillPrice)
	}
	if want := "任务失败：" + string(domain.CodeUpstreamRateLimited); entries[1].Reason != want {
		t.Errorf("退款理由 = %q, want %q", entries[1].Reason, want)
	}

	task, err := mysql.NewTaskRepo(f.db).Get(ctx, entries[0].TaskID)
	requireNoTestErr(t, err, "reload task")
	if task.Status != domain.TaskStatusFailed {
		t.Errorf("task status = %q, want failed", task.Status)
	}
}

// TestStoryboardBillRefundsUnparsableReply 走的是另一条退款路径：上游回了
// 200，钱已经花了，但回复解析不出镜头——产物拿不到，同样全额退。
func TestStoryboardBillRefundsUnparsableReply(t *testing.T) {
	f := newBillFixture(t, 100)
	f.driver.reply = "抱歉，我不太理解你的要求。"
	ctx := context.Background()

	_, _, _, bill, err := f.srv.generateStoryboard(ctx, f.call(), "第一场 巷口，夜。", 3, nil, nil)
	if err == nil {
		t.Fatal("回复解析不出镜头却返回了成功")
	}
	if bill != nil {
		t.Error("失败却返回了 bill")
	}
	if f.driver.calls != 1 {
		t.Errorf("上游调用次数 = %d, want 1", f.driver.calls)
	}
	if bal := f.balance(t); bal.Available != 100 || bal.Held != 0 {
		t.Errorf("退款后余额 = %+v, want available=100 held=0", bal)
	}
	entries := f.ledger(t)
	if len(entries) != 2 || entries[1].Type != "refund" {
		t.Fatalf("want hold + refund，实际：%+v", entries)
	}
}

// TestRefineBillChargesAfterDelivery 单钉同步改写链路：它 200 直接回正文，
// 冻结与结算都在这一次请求内闭环，不像出片那样有 worker 兜底。
func TestRefineBillChargesAfterDelivery(t *testing.T) {
	f := newBillFixture(t, 100)
	ctx := context.Background()

	text := "第一场 巷口，夜。"
	card := domain.Card{ID: uid.New(), Kind: domain.CardKindScript, Title: "旧标题", Text: &text}

	_, _, bill, err := f.srv.refineScript(ctx, f.call(), card, "把主角换成女性")
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	if bal := f.balance(t); bal.Held != chatBillPrice {
		t.Fatalf("改写返回时冻结额 = %d, want %d", bal.Held, chatBillPrice)
	}

	bill.charge(ctx)

	bal := f.balance(t)
	if bal.Available != 100-chatBillPrice || bal.Held != 0 {
		t.Errorf("结算后余额 = %+v, want available=%d held=0", bal, 100-chatBillPrice)
	}
	entries := f.ledger(t)
	t.Logf("credit_ledger for user %s: %+v", f.userID, entries)
	if len(entries) != 2 || entries[1].Type != "charge" {
		t.Fatalf("want hold + charge，实际：%+v", entries)
	}
}
