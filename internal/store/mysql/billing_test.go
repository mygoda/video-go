package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// TestLedgerHoldCharge 钉住记账的主干路径，其中最要紧的一条是：
// **actual == held 时冻结额度必须归零**。
//
// charge 那条流水的 amount 是"退回多冻结的部分"（held − actual），
// 两者相等时它是 0；若释放量从 amount 推导，这笔冻结就永远挂在 credits_held 上，
// 用户每成功一个任务可用余额就凭空少一截。
func TestLedgerHoldCharge(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	l := NewLedger(db)
	task := f.newTask(t, 30)

	entry, err := l.Hold(ctx, f.userID, task.ID, 30)
	requireNoErr(t, err, "hold")
	// hold 是出账形态，落库记为负。
	if entry.Amount != -30 {
		t.Errorf("hold amount = %d, want -30", entry.Amount)
	}
	if entry.Type != domain.LedgerHold {
		t.Errorf("entry type = %q", entry.Type)
	}

	b, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance after hold")
	if b.Available != 70 || b.Held != 30 {
		t.Fatalf("balance after hold = %+v, want available 70 / held 30", b)
	}

	// 全额结算：amount 为 0，但那笔冻结确实变成了实扣，必须从 credits_held 上消失。
	charge, err := l.Charge(ctx, f.userID, task.ID, 30)
	requireNoErr(t, err, "charge the full hold")
	if charge.Amount != 0 {
		t.Errorf("charge amount = %d, want 0 when actual equals the hold", charge.Amount)
	}

	b, err = l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance after charge")
	if b.Held != 0 {
		t.Errorf("credits_held = %d after a fully-settled task; the hold was never released", b.Held)
	}
	if b.Available != 70 {
		t.Errorf("available = %d after charge, want 70", b.Available)
	}

	// 重复的失败/成功回调是常态（webhook 与轮询同时判定），必须挡住。
	requireCode(t, func() error { _, err := l.Charge(ctx, f.userID, task.ID, 1); return err }(), domain.CodeConflict)
	requireCode(t, func() error { _, err := l.Refund(ctx, f.userID, task.ID, "dup"); return err }(), domain.CodeConflict)
}

// TestLedgerRefundSettlesOnce 钉住取消任务时那场真实存在的竞争的结果：
// HTTP 取消接口与 executor 发现终态后**都会**调 Refund，谁先到谁退成，
// 晚到的那条撞上幂等闸——只有一笔 refund 落库，余额精确。
//
// 这里按线上实际观察到的顺序串行发两笔（executor 先、HTTP 晚一步），而不是
// 起两个 goroutine 抢：两条路径隔着一次 HTTP 往返，真正同一瞬间进入事务
// 是另一码事，而那一码事目前挡不住（见随本票提交的说明）。本用例要钉的是
// 「晚到的那笔不改变余额，且带得出 ErrAlreadySettled」。
func TestLedgerRefundSettlesOnce(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	l := NewLedger(db)
	task := f.newTask(t, 8)

	_, err := l.Hold(ctx, f.userID, task.ID, 8)
	requireNoErr(t, err, "hold")

	// executor 先到：reason 与线上那条落库的流水一致。
	_, err = l.Refund(ctx, f.userID, task.ID, "任务已取消")
	requireNoErr(t, err, "executor 的退款")

	before, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance after the winning refund")

	// HTTP 取消接口晚一步：撞上幂等闸。
	_, lateErr := l.Refund(ctx, f.userID, task.ID, "task canceled")
	// 对外仍是 409——这条修复不改语义，只改调用方怎么读它。
	requireCode(t, lateErr, domain.CodeConflict)
	if !errors.Is(lateErr, billing.ErrAlreadySettled) {
		t.Fatalf("幂等闸挡下的 conflict 必须带 ErrAlreadySettled，否则调用方无从与真故障分开：%v", lateErr)
	}

	// 只有一笔 refund 落库——「钱没有凭空变多」的直接证据。
	var refundRows, refundTotal int
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(amount), 0) FROM credit_ledger
		 WHERE user_id = ? AND task_id = ? AND type = 'refund'`,
		f.userID, task.ID).Scan(&refundRows, &refundTotal), "count refund rows")
	if refundRows != 1 {
		t.Fatalf("落库的 refund 有 %d 笔，幂等闸没挡住", refundRows)
	}
	if refundTotal != 8 {
		t.Errorf("退款金额合计 %d，应等于当初冻结的 8", refundTotal)
	}

	// 被挡下的那笔一分钱都不该动：冻结额度在第一笔里就释放了，账是平的。
	after, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance after the blocked refund")
	if after != before {
		t.Fatalf("被幂等闸挡下的退款改变了余额：%+v -> %+v", before, after)
	}
	if after.Available != 100 || after.Held != 0 {
		t.Fatalf("退款后余额 = %+v，应是 available 100 / held 0", after)
	}

	// 把流水打出来，便于人工核对「hold −8 / refund +8」这一对。
	rows, err := db.QueryContext(ctx,
		`SELECT type, amount, balance_after, reason FROM credit_ledger
		 WHERE user_id = ? AND task_id = ? ORDER BY id`, f.userID, task.ID)
	requireNoErr(t, err, "dump ledger")
	defer rows.Close()
	for rows.Next() {
		var typ, reason string
		var amount, balanceAfter int
		requireNoErr(t, rows.Scan(&typ, &amount, &balanceAfter, &reason), "scan ledger row")
		t.Logf("ledger: type=%-6s amount=%+d balance_after=%d reason=%s", typ, amount, balanceAfter, reason)
	}
	requireNoErr(t, rows.Err(), "iterate ledger")
}

// TestSettledConflictIsDistinguishableFromOtherConflicts 是判别精度的证明。
//
// 账本里 conflict 不止幂等闸一个来源：唯一键撞了（这里用重复的充值幂等键）
// 同样是 409，但那是一次**真的没写进去**的写入。两者的 Code 一模一样，
// 只有错误链上的哨兵能把它们分开——调用方若按 Code 判，就会把真故障
// 当成"另一条路径已经做完了"静默掉。
func TestSettledConflictIsDistinguishableFromOtherConflicts(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	l := NewLedger(db)

	task := f.newTask(t, 10)
	_, err := l.Hold(ctx, f.userID, task.ID, 10)
	requireNoErr(t, err, "hold")
	_, err = l.Refund(ctx, f.userID, task.ID, "task canceled")
	requireNoErr(t, err, "first refund")

	_, settledErr := l.Refund(ctx, f.userID, task.ID, "任务已取消")
	requireCode(t, settledErr, domain.CodeConflict)
	if !errors.Is(settledErr, billing.ErrAlreadySettled) {
		t.Fatalf("幂等闸挡下的 conflict 必须带 ErrAlreadySettled，实际 %v", settledErr)
	}

	key := "topup_" + uid.Token(10)
	_, err = l.Topup(ctx, f.userID, 50, "seed", "", key)
	requireNoErr(t, err, "topup")
	_, dupErr := l.Topup(ctx, f.userID, 50, "seed", "", key)
	requireCode(t, dupErr, domain.CodeConflict)
	if errors.Is(dupErr, billing.ErrAlreadySettled) {
		t.Fatalf("唯一键冲突不是「已结算」，不该带 ErrAlreadySettled：%v", dupErr)
	}
}

// TestLedgerPartialChargeAndRefund 覆盖少收与全退两条分支。
func TestLedgerPartialChargeAndRefund(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	l := NewLedger(db)

	// 上游实际用量比预估少：少收，多冻的部分退回。
	cheap := f.newTask(t, 40)
	_, err := l.Hold(ctx, f.userID, cheap.ID, 40)
	requireNoErr(t, err, "hold")
	charge, err := l.Charge(ctx, f.userID, cheap.ID, 25)
	requireNoErr(t, err, "partial charge")
	if charge.Amount != 15 {
		t.Errorf("charge amount = %d, want the 15 that was over-held", charge.Amount)
	}
	b, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance")
	if b.Available != 75 || b.Held != 0 {
		t.Errorf("balance after partial charge = %+v, want available 75 / held 0", b)
	}

	// 失败全退：没拿到产物就不收钱。
	failed := f.newTask(t, 20)
	_, err = l.Hold(ctx, f.userID, failed.ID, 20)
	requireNoErr(t, err, "hold")
	refund, err := l.Refund(ctx, f.userID, failed.ID, "content_rejected")
	requireNoErr(t, err, "refund")
	if refund.Amount != 20 {
		t.Errorf("refund amount = %d, want the full 20 that was held", refund.Amount)
	}
	b, err = l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance")
	if b.Available != 75 || b.Held != 0 {
		t.Errorf("a failed task changed the balance: %+v, want available 75 / held 0", b)
	}

	// 用户看到的预估价就是他同意支付的上限，超出部分平台自己吃。
	over := f.newTask(t, 10)
	_, err = l.Hold(ctx, f.userID, over.ID, 10)
	requireNoErr(t, err, "hold")
	requireCode(t, func() error { _, err := l.Charge(ctx, f.userID, over.ID, 11); return err }(),
		domain.CodeInvalidParam)
}

// TestLedgerInsufficientCredit 钉住余额不足时任务根本不入队。
func TestLedgerInsufficientCredit(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 10, 1<<30)
	l := NewLedger(db)
	task := f.newTask(t, 50)

	requireCode(t, func() error { _, err := l.Hold(ctx, f.userID, task.ID, 50); return err }(),
		domain.CodeInsufficientCredit)

	// 被拒的 hold 不该留下任何痕迹。
	b, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance")
	if b.Available != 10 || b.Held != 0 {
		t.Errorf("a rejected hold moved the balance: %+v", b)
	}

	// 余额 10 分时并发提交的两个 10 分任务只有第一个能过。
	first := f.newTask(t, 10)
	second := f.newTask(t, 10)
	_, err = l.Hold(ctx, f.userID, first.ID, 10)
	requireNoErr(t, err, "first hold")
	requireCode(t, func() error { _, err := l.Hold(ctx, f.userID, second.ID, 10); return err }(),
		domain.CodeInsufficientCredit)

	requireCode(t, func() error { _, err := l.Hold(ctx, uid.New(), task.ID, 1); return err }(),
		domain.CodeNotFound)
}

// TestLedgerTopupIdempotency 钉住管理员充值的防重复点击。
func TestLedgerTopupIdempotency(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	l := NewLedger(db)
	repo := NewLedgerRepo(db)

	key := "topup_" + uid.Token(10)
	entry, err := l.Topup(ctx, f.userID, 1000, "manual topup", "", key)
	requireNoErr(t, err, "topup")
	if entry.Amount != 1000 {
		t.Errorf("topup amount = %d", entry.Amount)
	}

	// 双击同一个"充 1000 分"按钮：第二次必须是 409，钱不能充两遍。
	requireCode(t, func() error { _, err := l.Topup(ctx, f.userID, 1000, "manual topup", "", key); return err }(),
		domain.CodeConflict)

	b, err := l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance")
	if b.Available != 1000 {
		t.Errorf("balance = %d after a double-clicked topup, want 1000", b.Available)
	}

	found, ok, err := repo.GetByIdempotencyKey(ctx, key)
	requireNoErr(t, err, "get by idempotency key")
	if !ok || found.ID != entry.ID {
		t.Errorf("idempotency lookup missed: ok=%v id=%q", ok, found.ID)
	}
	// "这个 key 用过吗"的答案是"没有"，那不是错误。
	if _, ok, err := repo.GetByIdempotencyKey(ctx, "never_"+uid.Token(8)); err != nil || ok {
		t.Errorf("unused key should report (_, false, nil): ok=%v err=%v", ok, err)
	}

	// 扣减走负数。
	_, err = l.Topup(ctx, f.userID, -400, "correction", "", "topup_"+uid.Token(10))
	requireNoErr(t, err, "negative topup")
	b, err = l.Balance(ctx, f.userID)
	requireNoErr(t, err, "balance")
	if b.Available != 600 {
		t.Errorf("balance = %d after a -400 adjustment, want 600", b.Available)
	}

	requireCode(t, func() error { _, err := l.Topup(ctx, f.userID, 100, "r", "", ""); return err }(),
		domain.CodeInvalidParam)
	requireCode(t, func() error { _, err := l.Topup(ctx, f.userID, 0, "r", "", uid.Token(8)); return err }(),
		domain.CodeInvalidParam)
}

// TestLedgerRepoAppendAndRecompute 钉住流水与 users.credits 同事务，以及重算能纠偏。
//
// 起点刻意是 0 分再走一笔 topup 充进来，而不是直接给固件塞 credits：
// Recompute 是**纯粹从流水重算**的，直接写进 users 行的初始额度在流水里没有
// 对应记录，重算时就会被抹掉。这正是"流水是唯一真相、users.credits 只是
// 物化缓存"那条规则的直接后果，测试得按这个前提搭数据。
func TestLedgerRepoAppendAndRecompute(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	repo := NewLedgerRepo(db)

	_, err := NewLedger(db).Topup(ctx, f.userID, 100, "seed", "", "seed_"+uid.Token(10))
	requireNoErr(t, err, "seed credits through the ledger")

	e, err := repo.Append(ctx, domain.LedgerEntry{
		UserID: f.userID,
		Type:   domain.LedgerAdjust,
		Amount: 50,
		Reason: "manual adjust",
	})
	requireNoErr(t, err, "append")
	// balance_after 由事务内锁住用户行后算出，不信调用方传的值。
	if e.BalanceAfter != 150 {
		t.Errorf("balance_after = %d, want 150", e.BalanceAfter)
	}

	// 缓存必须已经跟着流水一起改了——两者分两次写，中间崩一次就永久分叉。
	var credits int
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT credits FROM users WHERE id = ?`, f.userID).Scan(&credits), "read credits")
	if credits != 150 {
		t.Errorf("users.credits = %d, want 150; the cache was not updated in the same transaction", credits)
	}

	requireCode(t, func() error {
		_, err := repo.Append(ctx, domain.LedgerEntry{UserID: f.userID, Type: "bogus", Amount: 1})
		return err
	}(), domain.CodeInvalidParam)

	// 把缓存手工搞歪，模拟崩溃留下的漂移。
	_, err = db.ExecContext(ctx,
		`UPDATE users SET credits = 999999, credits_held = 42 WHERE id = ?`, f.userID)
	requireNoErr(t, err, "corrupt the cache")
	b, err := repo.Recompute(ctx, f.userID)
	requireNoErr(t, err, "recompute")
	if b.Available != 150 {
		t.Errorf("recomputed available = %d, want 150", b.Available)
	}
	// 没有未结算的 hold，冻结额度该是 0。
	if b.Held != 0 {
		t.Errorf("recomputed held = %d, want 0", b.Held)
	}
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT credits FROM users WHERE id = ?`, f.userID).Scan(&credits), "read credits")
	if credits != 150 {
		t.Errorf("recompute did not write the truth back: %d", credits)
	}

	// 有一笔挂着的 hold 时，重算要认出它仍被冻结。
	task := f.newTask(t, 20)
	_, err = NewLedger(db).Hold(ctx, f.userID, task.ID, 20)
	requireNoErr(t, err, "hold")
	b, err = repo.Recompute(ctx, f.userID)
	requireNoErr(t, err, "recompute with an open hold")
	if b.Held != 20 {
		t.Errorf("recomputed held = %d, want the 20 still on hold", b.Held)
	}

	// 结算之后就不再算作冻结。charge 允许少收，因此这里不能用
	// "hold 之和减 charge 之和"来算——那个差额不是仍被冻结的钱。
	_, err = NewLedger(db).Charge(ctx, f.userID, task.ID, 5)
	requireNoErr(t, err, "charge")
	b, err = repo.Recompute(ctx, f.userID)
	requireNoErr(t, err, "recompute after settlement")
	if b.Held != 0 {
		t.Errorf("a settled task still counts as held: %d", b.Held)
	}

	requireCode(t, func() error { _, err := repo.Recompute(ctx, uid.New()); return err }(), domain.CodeNotFound)
}

// TestLedgerRepoListPaging 钉住流水按 id DESC 的游标分页。
func TestLedgerRepoListPaging(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	repo := NewLedgerRepo(db)

	const total = 5
	for i := 0; i < total; i++ {
		_, err := repo.Append(ctx, domain.LedgerEntry{
			UserID: f.userID, Type: domain.LedgerTopup, Amount: 10, Reason: "seed",
		})
		requireNoErr(t, err, "append")
	}

	seen := map[string]bool{}
	var lastID string
	cursor := ""
	for page := 0; page < 10; page++ {
		p, err := repo.List(ctx, f.userID, cursor, 2)
		requireNoErr(t, err, "list ledger")
		for _, e := range p.Items {
			if seen[e.ID] {
				t.Fatalf("entry %s appeared on two pages", e.ID)
			}
			seen[e.ID] = true
			// id 是 UUIDv7，id 序即时间序；DESC 分页必须严格递减。
			if lastID != "" && e.ID >= lastID {
				t.Errorf("ledger is not ordered by id DESC: %q came after %q", e.ID, lastID)
			}
			lastID = e.ID
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged over %d entries, want %d", len(seen), total)
	}

	p, err := repo.List(ctx, f.userID, "!!!garbage!!!", 100)
	requireNoErr(t, err, "list with garbage cursor")
	if len(p.Items) != total {
		t.Errorf("garbage cursor returned %d entries, want %d", len(p.Items), total)
	}
}

// TestQueueClaim 钉住租约领取，其中读回那一步是这条路径最容易静默失效的地方：
// lease_expires_at 是 DATETIME(3)，写进去的时刻若带着微秒，
// 按同一个变量读回就匹配不上，Claim 会在明明已经打上租约的情况下报告"没领到"。
func TestQueueClaim(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	q := NewQueue(db)
	task := f.newTask(t, 1)

	owner := "worker-" + uid.Token(8)
	lease, ok, err := q.Claim(ctx, owner, time.Minute, []string{f.providerID})
	requireNoErr(t, err, "claim")
	if !ok {
		t.Fatal("Claim reported no task while a queued one exists; the lease read-back did not match")
	}
	if lease.Task.ID != task.ID {
		t.Errorf("claimed %q, want %q", lease.Task.ID, task.ID)
	}
	if lease.Owner != owner {
		t.Errorf("lease owner = %q", lease.Owner)
	}
	if lease.ExpiresAt.IsZero() || lease.ExpiresAt.Before(time.Now().UTC()) {
		t.Errorf("lease expires at %v, which is not in the future", lease.ExpiresAt)
	}
	// 领回来的任务要带齐执行所需的字段，否则 worker 还得再查一次库。
	if lease.Task.Prompt == "" || lease.Task.ModelID != f.modelID {
		t.Errorf("leased task is missing execution fields: %+v", lease.Task)
	}

	// 已经被租走的任务不该被第二个 worker 领到。
	if _, ok, err := q.Claim(ctx, "worker-"+uid.Token(8), time.Minute, []string{f.providerID}); err != nil || ok {
		t.Errorf("a leased task was claimed twice: ok=%v err=%v", ok, err)
	}

	// 供应商列表为空表示"全部熔断"，不是"不限供应商"——
	// 后者会在熔断全开时把任务照领不误，正好抵消掉故障隔离。
	if _, ok, err := q.Claim(ctx, "worker-x", time.Minute, nil); err != nil || ok {
		t.Errorf("empty provider list must claim nothing: ok=%v err=%v", ok, err)
	}

	// 队列空不是错误。
	if _, ok, err := q.Claim(ctx, "worker-y", time.Minute, []string{uid.New()}); err != nil || ok {
		t.Errorf("an empty queue must be (Lease{}, false, nil): ok=%v err=%v", ok, err)
	}

	requireNoErr(t, q.Renew(ctx, task.ID, owner, 2*time.Minute), "renew")
	// 只有持有者能续租，否则原持有者会继续往一个不归它管的任务上写。
	requireCode(t, q.Renew(ctx, task.ID, "someone-else", time.Minute), domain.CodeConflict)
	requireCode(t, q.Renew(ctx, uid.New(), owner, time.Minute), domain.CodeNotFound)

	// Release 之后任务回到可领取状态。
	requireNoErr(t, q.Release(ctx, task.ID, owner), "release")
	reclaimed, ok, err := q.Claim(ctx, "worker-"+uid.Token(8), time.Minute, []string{f.providerID})
	requireNoErr(t, err, "re-claim")
	if !ok || reclaimed.Task.ID != task.ID {
		t.Errorf("released task was not claimable again: ok=%v", ok)
	}

	// 不是持有者时静默返回：worker 优雅退出会挨个 Release，
	// 为几条刚被接管的任务报错只会让退出日志变成一片红。
	requireNoErr(t, q.Release(ctx, task.ID, "not-the-owner"), "release by a non-owner is a no-op")
	requireCode(t, q.Renew(ctx, task.ID, "", time.Minute), domain.CodeInvalidParam)
}

// TestQueueClaimExpiredLease 钉住崩溃恢复：租约过期后任务能被别人接手。
func TestQueueClaimExpiredLease(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	q := NewQueue(db)
	task := f.newTask(t, 1)

	dead := "dead-worker-" + uid.Token(8)
	_, ok, err := q.Claim(ctx, dead, time.Minute, []string{f.providerID})
	requireNoErr(t, err, "claim")
	if !ok {
		t.Fatal("could not claim the task to set up the test")
	}

	// 模拟持有者崩掉：它不会再来续租，租约就那么挂到过期。
	_, err = db.ExecContext(ctx,
		`UPDATE tasks SET lease_expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond), task.ID)
	requireNoErr(t, err, "expire the lease")

	// 租约过期是崩溃后任务能自愈的全部机制。
	lease, ok, err := q.Claim(ctx, "fresh-worker-"+uid.Token(8), time.Minute, []string{f.providerID})
	requireNoErr(t, err, "claim an expired lease")
	if !ok || lease.Task.ID != task.ID {
		t.Fatalf("an expired lease was not reclaimed: ok=%v", ok)
	}
}

// TestQueueClaimPoll 覆盖到点轮询任务的批量领取。
func TestQueueClaimPoll(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 100, 1<<30)
	q := NewQueue(db)
	repo := NewTaskRepo(db)
	task := f.newTask(t, 1)

	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil),
		"to running")

	// 还没到点，不该被领。
	requireNoErr(t, repo.SetNextPoll(ctx, task.ID, time.Now().UTC().Add(time.Hour)), "schedule far out")
	leases, err := q.ClaimPoll(ctx, "poller-"+uid.Token(6), time.Minute, 10)
	requireNoErr(t, err, "claim poll")
	for _, l := range leases {
		if l.Task.ID == task.ID {
			t.Error("a task whose next_poll_at is in the future was claimed for polling")
		}
	}

	requireNoErr(t, repo.SetNextPoll(ctx, task.ID, time.Now().UTC().Add(-time.Second)), "schedule due")
	owner := "poller-" + uid.Token(6)
	leases, err = q.ClaimPoll(ctx, owner, time.Minute, 10)
	requireNoErr(t, err, "claim poll")
	found := false
	for _, l := range leases {
		if l.Task.ID == task.ID {
			found = true
			if l.Owner != owner {
				t.Errorf("lease owner = %q, want %q", l.Owner, owner)
			}
			if l.Task.Status != domain.TaskStatusRunning {
				t.Errorf("ClaimPoll returned a %q task, want running", l.Task.Status)
			}
		}
	}
	if !found {
		t.Fatal("a due task was not claimed for polling; the lease read-back did not match")
	}

	// 已经租出去的不该被第二个 poller 再领一次。
	again, err := q.ClaimPoll(ctx, "poller-"+uid.Token(6), time.Minute, 10)
	requireNoErr(t, err, "second claim poll")
	for _, l := range again {
		if l.Task.ID == task.ID {
			t.Error("a leased task was claimed for polling twice")
		}
	}

	requireCode(t, func() error { _, err := q.ClaimPoll(ctx, "", time.Minute, 10); return err }(),
		domain.CodeInvalidParam)
}
