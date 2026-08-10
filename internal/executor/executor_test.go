package executor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

func runningTask() domain.Task {
	ref := "up-1"
	return domain.Task{
		ID:            "task-1",
		UserID:        "user-1",
		ModelID:       "model-1",
		Status:        domain.TaskStatusRunning,
		EstimatedCost: 30,
		UpstreamRef:   &ref,
	}
}

// TestFinishTransfersBeforeSucceeded 钉住整个代码库里最要紧的那条顺序不变量：
// 产物必须在任务被置为 succeeded 之前落到本地存储。
//
// 断言的是**事件顺序**而不是"两者都发生了"：后者在实现把两行代码调换位置时
// 依然会通过，而那次调换正是这个测试要防的事故。
func TestFinishTransfersBeforeSucceeded(t *testing.T) {
	tasks := newFakeTasks(runningTask())
	tr := &fakeTransferor{tasks: tasks, assets: []domain.Asset{{ID: "asset-1"}}}
	ledger := &fakeLedger{}

	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Transferor = tr
		d.Ledger = ledger
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	if err := s.finish(context.Background(), task, []adapter.ArtifactRef{{Kind: adapter.KindURL}}, adapter.PollRequest{}, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	trace := tasks.trace()
	ti := slices.Index(trace, "transfer")
	si := slices.Index(trace, "status:succeeded")
	if ti < 0 || si < 0 {
		t.Fatalf("转存与置终态都应发生，实际 trace=%v", trace)
	}
	if ti > si {
		t.Fatalf("顺序颠倒：转存必须先于置 succeeded，trace=%v", trace)
	}

	charges, refunds := ledger.counts()
	if len(charges) != 1 || charges[0] != 30 {
		t.Fatalf("成功应结算一次 30，实际 %v", charges)
	}
	if refunds != 0 {
		t.Fatalf("成功不该退款，实际退了 %d 次", refunds)
	}
}

// TestFinishTransferFailureRefundsAndDoesNotSucceed 验证转存失败时任务判失败
// 并全额退款——绝不能留下一条 succeeded 却没有本地字节的记录。
func TestFinishTransferFailureRefundsAndDoesNotSucceed(t *testing.T) {
	tasks := newFakeTasks(runningTask())
	tr := &fakeTransferor{tasks: tasks, err: errors.New("上游 URL 已过期")}
	ledger := &fakeLedger{}

	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Transferor = tr
		d.Ledger = ledger
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	if err := s.finish(context.Background(), task, []adapter.ArtifactRef{{Kind: adapter.KindURL}}, adapter.PollRequest{}, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusFailed {
		t.Fatalf("转存失败时任务应为 failed，实际 %s", got.Status)
	}
	if got.Error == nil || got.Error.Charged {
		t.Fatalf("失败必须显式标记未扣费，实际 %+v", got.Error)
	}
	charges, refunds := ledger.counts()
	if len(charges) != 0 {
		t.Fatalf("转存失败不该结算，实际 %v", charges)
	}
	if refunds != 1 {
		t.Fatalf("转存失败应退款一次，实际 %d", refunds)
	}
}

// TestFailAlwaysRefundsAndNeverCharges 遍历全部五个失败分类，验证
// 「只要没拿到产物就不收钱」这条规则没有任何按分类分支的余地。
func TestFailAlwaysRefundsAndNeverCharges(t *testing.T) {
	codes := []domain.TaskErrorCode{
		domain.TaskErrorInvalidParam,
		domain.TaskErrorUpstreamRateLimited,
		domain.TaskErrorContentRejected,
		domain.TaskErrorInsufficientCredit,
		domain.TaskErrorInternal,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			tasks := newFakeTasks(runningTask())
			ledger := &fakeLedger{}
			s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) { d.Ledger = ledger })

			task, _ := tasks.Get(context.Background(), "task-1")
			// 故意传一个 Charged=true 的错误，验证 fail 会强制覆写成 false。
			err := s.fail(context.Background(), task, domain.TaskStatusRunning,
				&domain.TaskError{Code: code, Message: "x", Charged: true})
			if err != nil {
				t.Fatalf("fail: %v", err)
			}

			got, _ := tasks.Get(context.Background(), "task-1")
			if got.Status != domain.TaskStatusFailed {
				t.Fatalf("状态应为 failed，实际 %s", got.Status)
			}
			if got.Error.Charged {
				t.Fatal("失败恒为未扣费，Charged 必须被服务端强制置 false")
			}
			charges, refunds := ledger.counts()
			if len(charges) != 0 {
				t.Fatalf("失败不该结算，实际 %v", charges)
			}
			if refunds != 1 {
				t.Fatalf("失败应退款一次，实际 %d", refunds)
			}
		})
	}
}

// TestUpdateStatusCASLoserIsNotAnError 验证 CAS 输掉的那一方安静退出。
//
// 轮询与 webhook 会并发把同一条任务判失败；若输的一方也走一遍退款，
// 用户会被退两次钱。
func TestUpdateStatusCASLoserIsNotAnError(t *testing.T) {
	tasks := newFakeTasks(runningTask())
	ledger := &fakeLedger{}
	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) { d.Ledger = ledger })

	task, _ := tasks.Get(context.Background(), "task-1")
	terr := &domain.TaskError{Code: domain.TaskErrorInternal, Message: "x"}

	if err := s.fail(context.Background(), task, domain.TaskStatusRunning, terr); err != nil {
		t.Fatalf("第一次 fail: %v", err)
	}
	// 第二个到达者手里的 task 快照还是 running，它的 CAS 必然失败。
	if err := s.fail(context.Background(), task, domain.TaskStatusRunning, terr); err != nil {
		t.Fatalf("CAS 输掉不该是错误，实际 %v", err)
	}

	_, refunds := ledger.counts()
	if refunds != 1 {
		t.Fatalf("并发判失败只应退款一次，实际 %d", refunds)
	}
}

// TestAdvanceIsIdempotentOnTerminalTask 验证终态不可逆：迟到的 webhook
// 不能把一条已经 succeeded 的任务打回 running。
func TestAdvanceIsIdempotentOnTerminalTask(t *testing.T) {
	task := runningTask()
	task.Status = domain.TaskStatusSucceeded
	tasks := newFakeTasks(task)

	drv := &stubDriver{polls: []stubPoll{{res: adapter.PollResult{Status: adapter.StatusRunning}}}}
	s := newTestService(t, tasks, drv, nil)

	for i := 0; i < 3; i++ {
		if err := s.Advance(context.Background(), "task-1", "running"); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}

	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusSucceeded {
		t.Fatalf("终态不可逆，实际被改成了 %s", got.Status)
	}
	if drv.pollCall != 0 {
		t.Fatalf("终态任务不该再去问上游，实际问了 %d 次", drv.pollCall)
	}
}

// TestAdvanceConvergesOnRepeatedSucceeded 验证重复投递同一个成功结果
// 只会转存一次、只会结算一次。
func TestAdvanceConvergesOnRepeatedSucceeded(t *testing.T) {
	tasks := newFakeTasks(runningTask())
	tr := &fakeTransferor{tasks: tasks, assets: []domain.Asset{{ID: "asset-1"}}}
	ledger := &fakeLedger{}
	drv := &stubDriver{polls: []stubPoll{
		{res: adapter.PollResult{Status: adapter.StatusSucceeded, Artifacts: []adapter.ArtifactRef{{Kind: adapter.KindURL}}}},
		{res: adapter.PollResult{Status: adapter.StatusSucceeded, Artifacts: []adapter.ArtifactRef{{Kind: adapter.KindURL}}}},
	}}

	s := newTestService(t, tasks, drv, func(d *Deps) {
		d.Transferor = tr
		d.Ledger = ledger
	})

	for i := 0; i < 2; i++ {
		if err := s.Advance(context.Background(), "task-1", "succeeded"); err != nil {
			t.Fatalf("Advance #%d: %v", i, err)
		}
	}

	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusSucceeded {
		t.Fatalf("应收敛到 succeeded，实际 %s", got.Status)
	}
	charges, _ := ledger.counts()
	if len(charges) != 1 {
		t.Fatalf("重复 Advance 只应结算一次，实际 %v", charges)
	}
	if n := len(slices.DeleteFunc(tasks.trace(), func(s string) bool { return s != "transfer" })); n != 1 {
		t.Fatalf("重复 Advance 只应转存一次，实际 %d 次", n)
	}
}

// TestAdvanceOutOfOrderRunningAfterSucceeded 验证乱序：succeeded 先到、
// running 后到时，最终状态仍是 succeeded。
func TestAdvanceOutOfOrderRunningAfterSucceeded(t *testing.T) {
	tasks := newFakeTasks(runningTask())
	tr := &fakeTransferor{tasks: tasks, assets: []domain.Asset{{ID: "a1"}}}
	drv := &stubDriver{polls: []stubPoll{
		{res: adapter.PollResult{Status: adapter.StatusSucceeded, Artifacts: []adapter.ArtifactRef{{Kind: adapter.KindURL}}}},
		{res: adapter.PollResult{Status: adapter.StatusRunning}},
	}}
	s := newTestService(t, tasks, drv, func(d *Deps) { d.Transferor = tr; d.Ledger = &fakeLedger{} })

	_ = s.Advance(context.Background(), "task-1", "succeeded")
	// 迟到的 running 回调。
	_ = s.Advance(context.Background(), "task-1", "running")

	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusSucceeded {
		t.Fatalf("迟到的 running 不能把终态打回去，实际 %s", got.Status)
	}
}

// TestCancelQueuedRefundsWithoutUpstream 验证 queued 取消直接置终态并全额退款，
// 且不去打扰上游（它还不知道有这回事）。
func TestCancelQueuedRefundsWithoutUpstream(t *testing.T) {
	task := runningTask()
	task.Status = domain.TaskStatusQueued
	task.UpstreamRef = nil
	tasks := newFakeTasks(task)
	ledger := &fakeLedger{}
	drv := &stubDriver{}

	s := newTestService(t, tasks, drv, func(d *Deps) { d.Ledger = ledger })

	if err := s.Cancel(context.Background(), "task-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusCanceled {
		t.Fatalf("应为 canceled，实际 %s", got.Status)
	}
	charges, refunds := ledger.counts()
	if len(charges) != 0 {
		t.Fatalf("取消不扣费，实际结算了 %v", charges)
	}
	if refunds != 1 {
		t.Fatalf("取消应全额退款，实际 %d 次", refunds)
	}
	if drv.cancelCount() != 0 {
		t.Fatal("queued 任务还没提交到上游，不该调上游取消")
	}
}

// TestCancelRunningAsksUpstreamThenMarksLocally 验证 running 取消先请求上游
// （驱动实现了 Canceler），然后照样本地置 canceled、不扣费。
func TestCancelRunningAsksUpstreamThenMarksLocally(t *testing.T) {
	tasks := newFakeTasks(runningTask())
	ledger := &fakeLedger{}
	drv := &stubDriver{}
	s := newTestService(t, tasks, drv, func(d *Deps) { d.Ledger = ledger })

	if err := s.Cancel(context.Background(), "task-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if drv.cancelCount() != 1 {
		t.Fatalf("驱动支持取消时应调上游一次，实际 %d", drv.cancelCount())
	}
	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusCanceled {
		t.Fatalf("应为 canceled，实际 %s", got.Status)
	}
	charges, refunds := ledger.counts()
	if len(charges) != 0 || refunds != 1 {
		t.Fatalf("running 取消也不扣费且要退款，实际 charges=%v refunds=%d", charges, refunds)
	}
}

// TestCancelTerminalTaskConflicts 验证已结束的任务不能被取消。
func TestCancelTerminalTaskConflicts(t *testing.T) {
	task := runningTask()
	task.Status = domain.TaskStatusSucceeded
	tasks := newFakeTasks(task)
	s := newTestService(t, tasks, &stubDriver{}, nil)

	err := s.Cancel(context.Background(), "task-1")
	var derr *domain.Error
	if !errors.As(err, &derr) || derr.Code != domain.CodeConflict {
		t.Fatalf("取消终态任务应返回 conflict，实际 %v", err)
	}
}

// TestSubmitRetryOnlyForRetryableCodes 验证自动重试只对可重试分类生效。
//
// content_rejected 重试多少次还是拒，而每次重试都可能是一次真实计费调用。
func TestSubmitRetryOnlyForRetryableCodes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCalls  int
		wantStatus domain.TaskStatus
	}{
		{
			name:      "content_rejected 不重试",
			err:       &domain.Error{Code: domain.CodeContentRejected, Message: "拒了"},
			wantCalls: 1,
		},
		{
			name:      "invalid_param 不重试",
			err:       &domain.Error{Code: domain.CodeInvalidParam, Message: "参数错"},
			wantCalls: 1,
		},
		{
			name:      "upstream_rate_limited 重试到用尽",
			err:       &domain.Error{Code: domain.CodeUpstreamRateLimited, Message: "429"},
			wantCalls: 3,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tasks := newFakeTasks(runningTask())
			drv := &stubDriver{submits: []stubSubmit{
				{err: c.err}, {err: c.err}, {err: c.err}, {err: c.err},
			}}
			s := newTestService(t, tasks, drv, func(d *Deps) {
				// 3 次尝试、零退避：测试关心的是次数，不是等待时长。
				d.SubmitRetry = RetryPolicy{MaxAttempts: 3, BaseDelay: 0, MaxDelay: 0, Multiplier: 1}
				d.Ledger = &fakeLedger{}
			})

			task, _ := tasks.Get(context.Background(), "task-1")
			model, _ := stubModels{}.Get(context.Background(), "model-1")
			_, terr := s.submitWithRetry(context.Background(), task,
				model, adapter.SubmitInput{Provider: domain.Provider{ID: "prov-1"}}, adapter.PollRequest{})
			if terr == nil {
				t.Fatal("应当失败")
			}
			if terr.Charged {
				t.Fatal("失败必须标记未扣费")
			}
			if got := drv.submitCount(); got != c.wantCalls {
				t.Fatalf("调用次数应为 %d，实际 %d", c.wantCalls, got)
			}
		})
	}
}

// TestIsRetryable 钉住自动重试的分类表。
//
// 特别验证它**不看** TaskError.Retryable：invalid_param 对前端是
// retryable=true（"你改完参数再提交"），但对机器是不可重试的。
func TestIsRetryable(t *testing.T) {
	retryable := []domain.TaskErrorCode{
		domain.TaskErrorUpstreamRateLimited,
		domain.TaskErrorInternal,
	}
	notRetryable := []domain.TaskErrorCode{
		domain.TaskErrorInvalidParam,
		domain.TaskErrorContentRejected,
		domain.TaskErrorInsufficientCredit,
		domain.TaskErrorCode("未来才有的分类"),
	}
	for _, c := range retryable {
		if !IsRetryable(c) {
			t.Errorf("%s 应可重试", c)
		}
	}
	for _, c := range notRetryable {
		if IsRetryable(c) {
			t.Errorf("%s 不应重试", c)
		}
	}
}

// TestRetryPolicyDelayGrowsAndCaps 验证指数退避会增长、会封顶，且带抖动。
func TestRetryPolicyDelayGrowsAndCaps(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 10, BaseDelay: time.Second, MaxDelay: 8 * time.Second, Multiplier: 2, Jitter: 0}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := p.Delay(i + 1); got != w {
			t.Fatalf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}

	// 抖动必须真的抖：没有它，供应商恢复的瞬间全部积压任务会在同一毫秒重试。
	jittered := RetryPolicy{MaxAttempts: 10, BaseDelay: time.Second, MaxDelay: time.Second, Multiplier: 1, Jitter: 0.5}
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 40; i++ {
		seen[jittered.Delay(1)] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("Jitter > 0 时不同次调用必须给出不同的等待时长")
	}
}

// TestShouldRetryRespectsExhaustion 验证尝试次数用尽后不再重试。
func TestShouldRetryRespectsExhaustion(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3}
	rate := &domain.TaskError{Code: domain.TaskErrorUpstreamRateLimited}

	if !p.ShouldRetry(1, rate) || !p.ShouldRetry(2, rate) {
		t.Fatal("未用尽时应继续重试")
	}
	if p.ShouldRetry(3, rate) {
		t.Fatal("attempt == MaxAttempts 即为用尽")
	}
	// nil 表示"调用失败了但没归类"（网络错、超时），按可重试处理。
	if !p.ShouldRetry(1, nil) {
		t.Fatal("未归类的失败应按可重试处理")
	}
}

// TestPollIntervalPrecedence 验证间隔的优先级：模型配置 → 驱动建议 → 全局兜底。
func TestPollIntervalPrecedence(t *testing.T) {
	drv := &stubDriver{}
	s := newTestService(t, newFakeTasks(), drv, func(d *Deps) { d.DefaultPollInterval = 99 * time.Second })

	vp := domain.VideoProtocolArk
	seconds := 42
	withCfg := domain.ModelConfig{Family: domain.FamilyVideo, VideoProtocol: &vp, PollIntervalSeconds: &seconds}
	if got := s.pollInterval(withCfg, adapter.PollRequest{}); got != 42*time.Second {
		t.Fatalf("模型配置优先，want 42s got %v", got)
	}

	noCfg := domain.ModelConfig{Family: domain.FamilyVideo, VideoProtocol: &vp}
	if got := s.pollInterval(noCfg, adapter.PollRequest{}); got != 5*time.Second {
		t.Fatalf("应回落到驱动建议 5s，got %v", got)
	}

	unknown := domain.ModelConfig{Family: domain.FamilyChat}
	if got := s.pollInterval(unknown, adapter.PollRequest{}); got != 99*time.Second {
		t.Fatalf("应回落到全局兜底 99s，got %v", got)
	}
}

// TestInternalErrorLogsCauseWithTaskID 钉住失败的可定位性。
//
// 对用户那句话是刻意脱敏的（「上游调用失败，请重试」），根因不进 Message——
// 那是对的。代价是排查时手上只剩这句话，于是根因必须在日志里、且必须挂着
// task_id：没有这个钩子，用户报上来一句「解析输入素材失败」之后，
// 就只能按时间在整个日志里大海捞针。DEM-94 那次 InnoDB 死锁正是这么被埋掉的。
func TestInternalErrorLogsCauseWithTaskID(t *testing.T) {
	var buf bytes.Buffer
	tasks := newFakeTasks(runningTask())
	cause := errors.New("Error 1213 (40001): Deadlock found when trying to get lock")
	drv := &stubDriver{submits: []stubSubmit{{err: cause}}}
	s := newTestService(t, tasks, drv, func(d *Deps) {
		d.SubmitRetry = RetryPolicy{MaxAttempts: 1, BaseDelay: 0, MaxDelay: 0, Multiplier: 1}
		d.Ledger = &fakeLedger{}
		d.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	model, _ := stubModels{}.Get(context.Background(), "model-1")
	_, terr := s.submitWithRetry(context.Background(), task,
		model, adapter.SubmitInput{Provider: domain.Provider{ID: "prov-1"}}, adapter.PollRequest{})
	if terr == nil {
		t.Fatal("应当失败")
	}

	// 对外那句话仍然不许带根因：它会显示给用户，泄露内部拓扑。
	if strings.Contains(terr.Message, cause.Error()) {
		t.Errorf("对外文案泄露了根因：%q", terr.Message)
	}

	logged := buf.String()
	if !strings.Contains(logged, `"task_id":"task-1"`) {
		t.Errorf("日志里没有 task_id，失败与根因对不上：\n%s", logged)
	}
	if !strings.Contains(logged, cause.Error()) {
		t.Errorf("日志里没有根因 error：\n%s", logged)
	}
	t.Logf("失败日志：\n%s", logged)
}
