package mysql

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// settlementRow 是 credit_ledger 里一条结算流水的可打印形态，
// 并发用例失败时要靠它逐行说明"钱是怎么多出来的"。
type settlementRow struct {
	Type         string
	Amount       int
	BalanceAfter int
	Reason       string
	IdemKey      string
}

// dumpTaskLedger 打出某任务名下的全部流水，并单独返回结算类（charge/refund）的那些。
//
// idempotency_key 一并打出来：并发下的幂等闸就建在它的唯一键上，
// 出问题时第一件要看的就是结算流水到底有没有带上这个键。
func dumpTaskLedger(t *testing.T, ctx context.Context, userID, taskID string) []settlementRow {
	t.Helper()
	db := requireDB(t)
	rows, err := db.QueryContext(ctx,
		`SELECT type, amount, balance_after, COALESCE(reason, ''), COALESCE(idempotency_key, '')
		 FROM credit_ledger
		 WHERE user_id = ? AND task_id = ? ORDER BY id`, userID, taskID)
	requireNoErr(t, err, "dump task ledger")
	defer rows.Close()

	var settlements []settlementRow
	for rows.Next() {
		var r settlementRow
		requireNoErr(t, rows.Scan(&r.Type, &r.Amount, &r.BalanceAfter, &r.Reason, &r.IdemKey),
			"scan ledger row")
		t.Logf("  ledger %-6s amount=%+d balance_after=%d idem_key=%q reason=%s",
			r.Type, r.Amount, r.BalanceAfter, r.IdemKey, r.Reason)
		if r.Type == string(domain.LedgerCharge) || r.Type == string(domain.LedgerRefund) {
			settlements = append(settlements, r)
		}
	}
	requireNoErr(t, rows.Err(), "iterate task ledger")
	return settlements
}

// requireNoLockFailure 把 1213（死锁）/ 1205（锁等待超时）单独拎出来当失败报。
//
// 这两个错码是修法把「判定」串行化时最可能引入的副作用，若被当成
// 「另一条路径已经结算过了」静默掉，用户那笔退款就真的丢了。
func requireNoLockFailure(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		return
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205) {
		t.Fatalf("%s 撞上了锁故障（%d），修法引入了新的加锁顺序问题：%v", what, me.Number, err)
	}
}

// TestConcurrentRefundDoubleSpends 是这张票的核心复现：取消一次任务，
// HTTP 侧与 executor 侧**同一瞬间**各发一笔退款。
//
// 基线上两笔都能通过幂等闸——REPEATABLE READ 下 taskHoldTx 那条不加锁的
// SELECT 把事务快照钉在了对手写入之前，等 appendLedgerTx 拿到用户行的
// FOR UPDATE 时，判定早就做完了。结果是 refund 落库两笔、用户凭空多一份钱。
func TestConcurrentRefundDoubleSpends(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	l := NewLedger(db)
	task := f.newTask(t, 8)

	before, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "起点余额")
	if _, err := l.Hold(ctx, f.userID, task.ID, 8); err != nil {
		t.Fatalf("hold: %v", err)
	}

	// 两个 reason 就是线上那两条路径各自写的字面量：
	// httpapi 的 cancelTask 写 "task canceled"，executor 的 refund 写 "任务已取消"。
	reasons := []string{"task canceled", "任务已取消"}
	errs := make([]error, len(reasons))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, reason := range reasons {
		wg.Add(1)
		go func(i int, reason string) {
			defer wg.Done()
			<-start
			_, errs[i] = l.Refund(ctx, f.userID, task.ID, reason)
		}(i, reason)
	}
	close(start)
	wg.Wait()

	var winners int
	for i, err := range errs {
		t.Logf("refund[%s] -> %v", reasons[i], err)
		requireNoLockFailure(t, err, "并发退款")
		if err == nil {
			winners++
			continue
		}
		// 挡下来的那笔必须仍然是「已结算」，否则 publish.go / runner.go
		// 的日志分级会把它退回「误报需人工对账」（DEM-97）。
		requireCode(t, err, domain.CodeConflict)
		if !errors.Is(err, billing.ErrAlreadySettled) {
			t.Fatalf("被挡下的退款必须带 billing.ErrAlreadySettled：%v", err)
		}
	}

	settlements := dumpTaskLedger(t, ctx, f.userID, task.ID)
	if winners != 1 {
		t.Errorf("并发退款成功了 %d 笔，期望恰好 1 笔", winners)
	}
	if len(settlements) != 1 {
		t.Errorf("结算流水落库 %d 笔（期望 1），幂等闸在并发下漏了", len(settlements))
	}

	after, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "退款后余额")
	if after.Available != before.Available {
		t.Errorf("钱凭空变了：退款后 available=%d，起点=%d（差 %+d）",
			after.Available, before.Available, after.Available-before.Available)
	}
	if after.Held != 0 {
		t.Errorf("退款后 credits_held = %d，冻结额没被释放干净", after.Held)
	}
}

// TestConcurrentChargeAndRefundSettleOnce 覆盖 Charge——它走的是**同一个**
// taskHoldTx，因此同一个洞。
//
// 并发的一次 charge 与一次 refund 在基线上会双双通过：同一笔钱既被实扣
// 又被全额退回，credits_held 还会被释放两遍。只允许一笔结算落库，
// 且余额必须与那一笔的语义严格对上。
func TestConcurrentChargeAndRefundSettleOnce(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	l := NewLedger(db)
	task := f.newTask(t, 8)

	before, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "起点余额")
	if _, err := l.Hold(ctx, f.userID, task.ID, 8); err != nil {
		t.Fatalf("hold: %v", err)
	}

	const actual = 5
	var (
		wg                   sync.WaitGroup
		chargeErr, refundErr error
		start                = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, chargeErr = l.Charge(ctx, f.userID, task.ID, actual)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, refundErr = l.Refund(ctx, f.userID, task.ID, "任务已取消")
	}()
	close(start)
	wg.Wait()

	t.Logf("charge -> %v", chargeErr)
	t.Logf("refund -> %v", refundErr)
	requireNoLockFailure(t, chargeErr, "并发 charge")
	requireNoLockFailure(t, refundErr, "并发 refund")

	// 恰好一胜一负，且败的那笔带得出「已结算」。
	loser := chargeErr
	if chargeErr == nil {
		loser = refundErr
	}
	if (chargeErr == nil) == (refundErr == nil) {
		t.Errorf("charge/refund 并发结果不是一胜一负：charge=%v refund=%v", chargeErr, refundErr)
	} else {
		requireCode(t, loser, domain.CodeConflict)
		if !errors.Is(loser, billing.ErrAlreadySettled) {
			t.Fatalf("被挡下的那笔必须带 billing.ErrAlreadySettled：%v", loser)
		}
	}

	settlements := dumpTaskLedger(t, ctx, f.userID, task.ID)
	if len(settlements) != 1 {
		t.Fatalf("结算流水落库 %d 笔（期望 1）：同一笔钱既扣又退", len(settlements))
	}

	// 余额守恒：charge 赢就少收 actual，refund 赢就一分不收。两种都要求冻结归零。
	wantAvailable := before.Available
	if chargeErr == nil {
		wantAvailable -= actual
	}
	after, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "结算后余额")
	if after.Available != wantAvailable {
		t.Errorf("结算后 available=%d，期望 %d（起点 %d，charge %s）",
			after.Available, wantAvailable, before.Available,
			map[bool]string{true: "胜", false: "负"}[chargeErr == nil])
	}
	if after.Held != 0 {
		t.Errorf("结算后 credits_held = %d，冻结额没被释放干净", after.Held)
	}
}

// TestTopupRejectsSettleNamespace 守住结算键的命名空间。
//
// 管理员充值的幂等键是**他自己填的**，而结算流水现在也写同一个唯一键。
// 若能提交一个 "settle:<task-id>" 的充值键，那个任务的结算就被永久顶掉：
// 冻结额再也释放不了，用户的钱卡死在 credits_held 上。
func TestTopupRejectsSettleNamespace(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	l := NewLedger(db)
	task := f.newTask(t, 8)

	poison := settleIdempotencyKey(task.ID)
	requireCode(t, func() error { _, err := l.Topup(ctx, f.userID, 100, "poison", "", poison); return err }(),
		domain.CodeInvalidParam)

	// 被拒之后这个任务照样结算得掉。
	_, err := l.Hold(ctx, f.userID, task.ID, 0)
	requireNoErr(t, err, "hold")
	_, err = l.Refund(ctx, f.userID, task.ID, "task canceled")
	requireNoErr(t, err, "refund 仍然可用")
}
