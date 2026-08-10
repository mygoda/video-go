package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// logCapture 收集 slog 记录。logRefund 走的是包级 slog，因此这里换掉默认
// handler 而不是注入 logger。
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

// captureLogs 把默认 logger 换成捕获器，并在用例结束时还原。
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// TestLogRefundAlreadySettledIsNotAnIncident 钉住误报修复：
// 取消任务时 HTTP 接口与 executor 都会调 Refund，晚到的那条撞上账本的幂等闸，
// 而 settled 为真恰恰证明冻结额度已经被前一次结算释放掉了——钱没被冻住，
// 没有账要对。
//
// 断言级别与措辞而不是"没 panic"：这条路径的产物就是那行日志。
func TestLogRefundAlreadySettledIsNotAnIncident(t *testing.T) {
	c := captureLogs(t)
	s := &server{}

	s.logRefund("task-1", &domain.Error{
		Code:    domain.CodeConflict,
		Message: "task task-1 has already been settled",
		Err:     billing.ErrAlreadySettled,
	})

	got := c.snapshot()
	if len(got) != 1 {
		t.Fatalf("应恰好留一条记录说明跳过了什么，实际 %d 条", len(got))
	}
	if got[0].Level >= slog.LevelError {
		t.Errorf("幂等闸挡下的重复退款不是事故，级别应低于 ERROR，实际 %s", got[0].Level)
	}
	if strings.Contains(got[0].Message, "需人工对账") {
		t.Errorf("「需人工对账」只留给真事故，不该出现在 %q 里", got[0].Message)
	}
}

// TestLogRefundRealFailureStillDemandsReconciliation 是本次修复唯一不许弄丢的
// 东西：底层真的没退成时钱还冻着，必须保持 ERROR + 「需人工对账」。
func TestLogRefundRealFailureStillDemandsReconciliation(t *testing.T) {
	c := captureLogs(t)
	s := &server{}

	s.logRefund("task-1", &domain.Error{
		Code:      domain.CodeInternal,
		Message:   "storage: append ledger entry",
		Retryable: true,
		Err:       errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"),
	})

	got := c.snapshot()
	if len(got) != 1 {
		t.Fatalf("退款真失败应记且只记一条，实际 %d 条", len(got))
	}
	if got[0].Level != slog.LevelError {
		t.Errorf("退款真失败必须是 ERROR，实际 %s", got[0].Level)
	}
	if !strings.Contains(got[0].Message, "需人工对账") {
		t.Errorf("退款真失败必须带「需人工对账」，实际 %q", got[0].Message)
	}
}

// TestLogRefundNonSettledConflictStillDemandsReconciliation 证明判别的精度：
// 账本里 conflict 不止幂等闸一个来源——外键指向的行不存在、唯一键撞了同样被
// store/mysql 的 wrap 归成 conflict，而那些是真的没写进去、钱还冻着。
//
// 这个用例的错误码与幂等闸一模一样，只差错误链上没挂哨兵。它必须仍报 ERROR，
// 即"判别落在 errors.Is(哨兵) 上，不是 Code == conflict"。
func TestLogRefundNonSettledConflictStillDemandsReconciliation(t *testing.T) {
	c := captureLogs(t)
	s := &server{}

	s.logRefund("task-1", &domain.Error{
		Code:    domain.CodeConflict,
		Message: "append ledger entry: referenced row does not exist",
		Err:     errors.New("Error 1452: foreign key constraint fails"),
	})

	got := c.snapshot()
	if len(got) != 1 || got[0].Level != slog.LevelError ||
		!strings.Contains(got[0].Message, "需人工对账") {
		t.Fatalf("非幂等闸来源的 conflict 是真故障，必须保持 ERROR + 需人工对账，实际 %+v", got)
	}
}
