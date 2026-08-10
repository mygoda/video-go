package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// settledConflict 复刻账本幂等闸返回的那个错误：Code 是 conflict，
// 错误链上挂着 billing.ErrAlreadySettled。
//
// 手写而不是引 store/mysql：executor 不认识存储实现，它能依赖的只有
// billing.Ledger 这个契约，而契约里写明了"撞上已结算返回带该哨兵的 conflict"。
func settledConflict() error {
	return &domain.Error{
		Code:    domain.CodeConflict,
		Message: "task task-1 has already been settled",
		Err:     billing.ErrAlreadySettled,
	}
}

// TestRefundAlreadySettledIsNotAnIncident 钉住误报修复的正方向：
// 取消任务时 HTTP 接口与 executor 都会调 Refund，晚到的那条必然撞上幂等闸，
// 而那恰恰证明冻结额度已经被前一次结算释放掉了，钱一分没少。
//
// 断言的是**日志级别与措辞**而不是"没 panic"：这条路径的产物就是那行日志，
// 「需人工对账」是发给运维的行动指令，误发一次就有人去查一笔不存在的欠账。
func TestRefundAlreadySettledIsNotAnIncident(t *testing.T) {
	logs := &logCapture{}
	tasks := newFakeTasks(runningTask())
	ledger := &fakeLedger{refundErr: settledConflict()}
	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Ledger = ledger
		d.Logger = logs.logger()
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	s.refund(context.Background(), task, "任务已取消")

	if errs := logs.errorsLogged(); len(errs) != 0 {
		t.Fatalf("幂等闸挡下的重复退款不是事故，不该记 ERROR，实际记了 %v", errs)
	}
	below := logs.messagesBelowError()
	if len(below) == 0 {
		t.Fatal("重复退款应留一条低于 ERROR 的记录说明发生了什么，实际一条都没有")
	}
	for _, m := range below {
		if strings.Contains(m, "需人工对账") {
			t.Errorf("「需人工对账」只留给真事故，不该出现在 %q 里", m)
		}
	}
}

// TestRefundRealFailureStillDemandsReconciliation 钉住反方向，也是本次修复
// 唯一不许弄丢的东西：底层真的没退成（库挂了、超时）时，钱还冻着，
// 必须保持 ERROR + 「需人工对账」。
func TestRefundRealFailureStillDemandsReconciliation(t *testing.T) {
	logs := &logCapture{}
	tasks := newFakeTasks(runningTask())
	ledger := &fakeLedger{refundErr: &domain.Error{
		Code:      domain.CodeInternal,
		Message:   "storage: append ledger entry",
		Retryable: true,
		Err:       errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"),
	}}
	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Ledger = ledger
		d.Logger = logs.logger()
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	s.refund(context.Background(), task, "internal_error")

	errs := logs.errorsLogged()
	if len(errs) != 1 {
		t.Fatalf("退款真失败必须记且只记一条 ERROR，实际 %v", errs)
	}
	if !strings.Contains(errs[0], "需人工对账") {
		t.Errorf("退款真失败的 ERROR 必须带「需人工对账」，实际 %q", errs[0])
	}
}

// TestRefundNonSettledConflictStillDemandsReconciliation 是判别精度的证明：
// 账本里 conflict 不止幂等闸一个来源——外键指向的行不存在、唯一键撞了
// 都被 store/mysql 的 wrap 归成 conflict（那些是真的没写进去，钱还冻着）。
//
// 因此判别必须落在 errors.Is(哨兵) 上而不是 Code == conflict：
// 这个用例里的错误码与幂等闸完全一样，只差错误链上没挂哨兵，
// 而它必须仍然报 ERROR。
func TestRefundNonSettledConflictStillDemandsReconciliation(t *testing.T) {
	logs := &logCapture{}
	tasks := newFakeTasks(runningTask())
	ledger := &fakeLedger{refundErr: &domain.Error{
		Code:    domain.CodeConflict,
		Message: "append ledger entry: referenced row does not exist",
		Err:     errors.New("Error 1452: foreign key constraint fails"),
	}}
	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Ledger = ledger
		d.Logger = logs.logger()
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	s.refund(context.Background(), task, "internal_error")

	errs := logs.errorsLogged()
	if len(errs) != 1 || !strings.Contains(errs[0], "需人工对账") {
		t.Fatalf("非幂等闸来源的 conflict 是真故障，必须保持 ERROR + 需人工对账，实际 %v", errs)
	}
}

// TestChargeAlreadySettledIsNotAnIncident 覆盖结算侧的同一条误报：
// Charge 撞上已结算时冻结额度同样早已释放，账面是平的，没有可对的账。
func TestChargeAlreadySettledIsNotAnIncident(t *testing.T) {
	logs := &logCapture{}
	tasks := newFakeTasks(runningTask())
	tr := &fakeTransferor{tasks: tasks, assets: []domain.Asset{{ID: "asset-1"}}}
	ledger := &fakeLedger{chargeErr: settledConflict()}
	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Transferor = tr
		d.Ledger = ledger
		d.Logger = logs.logger()
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	if err := s.finish(context.Background(), task, []adapter.ArtifactRef{{Kind: adapter.KindURL}}, adapter.PollRequest{}, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if errs := logs.errorsLogged(); len(errs) != 0 {
		t.Fatalf("结算撞上幂等闸不是事故，不该记 ERROR，实际记了 %v", errs)
	}
	// 产物已经交付，任务仍必须停在 succeeded——日志的降级不该顺手改动结算语义。
	got, _ := tasks.Get(context.Background(), "task-1")
	if got.Status != domain.TaskStatusSucceeded {
		t.Fatalf("结算撞上幂等闸不该改判任务状态，实际 %s", got.Status)
	}
}

// TestChargeRealFailureStillDemandsReconciliation 是结算侧的反方向：
// 真的没结算成功时少收了一笔，必须留 ERROR 让人来对。
func TestChargeRealFailureStillDemandsReconciliation(t *testing.T) {
	logs := &logCapture{}
	tasks := newFakeTasks(runningTask())
	tr := &fakeTransferor{tasks: tasks, assets: []domain.Asset{{ID: "asset-1"}}}
	ledger := &fakeLedger{chargeErr: &domain.Error{
		Code:    domain.CodeInternal,
		Message: "storage: update credits cache",
		Err:     errors.New("invalid connection"),
	}}
	s := newTestService(t, tasks, &stubDriver{}, func(d *Deps) {
		d.Transferor = tr
		d.Ledger = ledger
		d.Logger = logs.logger()
	})

	task, _ := tasks.Get(context.Background(), "task-1")
	if err := s.finish(context.Background(), task, []adapter.ArtifactRef{{Kind: adapter.KindURL}}, adapter.PollRequest{}, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	errs := logs.errorsLogged()
	if !slices.ContainsFunc(errs, func(m string) bool { return strings.Contains(m, "需人工对账") }) {
		t.Fatalf("结算真失败必须带「需人工对账」，实际 %v", errs)
	}
}
