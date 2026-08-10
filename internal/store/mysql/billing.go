package mysql

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// ledger 是 billing.Ledger 的 MySQL 实现。
//
// 它落在本包而不是 internal/billing，是因为「流水与 users.credits 必须在
// 同一个事务里写」这条规则只有握着数据库连接的一侧能兑现——billing 包里
// 拼不出这个事务，只能把两次写分开发出去，而那正是它禁止的事。
type ledger struct{ db *sql.DB }

// NewLedger 构造 billing.Ledger。
func NewLedger(db *sql.DB) billing.Ledger { return &ledger{db: db} }

// Hold 冻结额度。
//
// 检查余额与占用余额是**同一个原子动作**：先 FOR UPDATE 锁住用户行再判断，
// 因此余额 10 分时并发提交的 10 个各 10 分的任务里只有第一个能通过，
// 后面 9 个看到的是已经被占掉的余额（见 internal/billing 的包注释）。
// 不够时返回 domain.CodeInsufficientCredit（HTTP 402），任务根本不会入队。
//
// amount 是正数形态的"要冻结多少"，落库时记为负（出账）。
func (l *ledger) Hold(ctx context.Context, userID, taskID string, amount int) (domain.LedgerEntry, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if amount < 0 {
		return domain.LedgerEntry{}, invalidParam("hold amount must not be negative")
	}

	entry := domain.LedgerEntry{
		ID:     uid.New(),
		UserID: userID,
		Type:   domain.LedgerHold,
		Amount: -amount,
		Reason: "task hold",
	}
	if taskID != "" {
		entry.TaskID = &taskID
	}

	err := withTx(ctx, l.db, func(tx *sql.Tx) error {
		var available int
		switch err := tx.QueryRowContext(ctx,
			`SELECT credits FROM users WHERE id = ? FOR UPDATE`, userID).Scan(&available); {
		case isNoRows(err):
			return notFound("user", userID)
		case err != nil:
			return wrap("lock user for hold", err)
		}
		if available < amount {
			return insufficientCredit(
				"balance " + strconv.Itoa(available) + " is below the required " + strconv.Itoa(amount))
		}
		// 冻结额度增加 amount：entry.Amount 是 -amount（出账形态）。
		_, err := appendLedgerTx(ctx, tx, &entry, amount)
		return err
	})
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	return entry, nil
}

// Charge 结算一笔冻结额度。
//
// actual 允许小于冻结额（上游实际用量比预估少时少收），**不允许大于**：
// 用户看到的预估价就是他同意支付的上限，超出部分只能平台自己承担。
// 这条规则在这里硬拦住而不是靠调用方自觉——多收钱是不可逆的用户信任损失。
//
// 记账形态：hold 时已经把 actual 对应的钱从 credits 里扣走了，因此 charge
// 只需要把多冻结的那部分退回（amount = hold − actual ≥ 0），
// 同时释放整笔冻结额度。
func (l *ledger) Charge(ctx context.Context, userID, taskID string, actual int) (domain.LedgerEntry, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if err := requireID("task id", taskID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if actual < 0 {
		return domain.LedgerEntry{}, invalidParam("charge amount must not be negative")
	}

	entry := domain.LedgerEntry{
		ID:     uid.New(),
		UserID: userID,
		TaskID: &taskID,
		Type:   domain.LedgerCharge,
		Reason: "task charge",
	}

	err := withTx(ctx, l.db, func(tx *sql.Tx) error {
		held, settled, err := taskHoldTx(ctx, tx, userID, taskID)
		if err != nil {
			return err
		}
		if settled {
			return conflict("task "+taskID+" has already been settled", billing.ErrAlreadySettled)
		}
		if actual > held {
			return invalidParam(
				"charge " + strconv.Itoa(actual) + " exceeds the held " + strconv.Itoa(held) +
					"; the quoted estimate is the agreed ceiling")
		}
		// amount 是"退回多冻结的部分"。actual == held 时为 0，流水仍要落一条：
		// 它是"这笔钱在什么时候被结算掉了"的凭证，缺了就无法逐条回放对账。
		//
		// 释放的冻结额是**整笔 held**，不是 amount：held == actual 时 amount 为 0，
		// 但那笔冻结确实已经变成实扣，再挂在 credits_held 上就是重复占用。
		entry.Amount = held - actual
		_, err = appendLedgerTx(ctx, tx, &entry, -held)
		return err
	})
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	return entry, nil
}

// Refund 释放冻结额度，失败或取消时调用。
//
// 全额退回：五类失败（invalid_param / upstream_rate_limited / content_rejected /
// insufficient_credit / internal_error）都不扣费，取消同理——
// 「只要没拿到产物就不收钱」（见 internal/billing 的包注释）。
func (l *ledger) Refund(ctx context.Context, userID, taskID string, reason string) (domain.LedgerEntry, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if err := requireID("task id", taskID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if reason == "" {
		reason = "task refund"
	}

	entry := domain.LedgerEntry{
		ID:     uid.New(),
		UserID: userID,
		TaskID: &taskID,
		Type:   domain.LedgerRefund,
		Reason: reason,
	}

	err := withTx(ctx, l.db, func(tx *sql.Tx) error {
		held, settled, err := taskHoldTx(ctx, tx, userID, taskID)
		if err != nil {
			return err
		}
		if settled {
			// 幂等的边界：已结算的任务再退一次会把钱凭空变多。
			// 重复的失败回调是常态（webhook + 轮询同时判失败），必须挡住。
			//
			// cause 带上 billing.ErrAlreadySettled，是为了让调用方能把这一种
			// conflict 与"真的没退成"分开——两者的 Code 都是 conflict。
			return conflict("task "+taskID+" has already been settled", billing.ErrAlreadySettled)
		}
		entry.Amount = held
		_, err = appendLedgerTx(ctx, tx, &entry, -held)
		return err
	})
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	return entry, nil
}

// Topup 是管理员手工充值（不接支付网关，PRD 定的）。
//
// amount 为正是充值、为负是扣减。idempotencyKey 必填：管理员双击一次
// "充 1000 分"就该只充一次，而这个防线只能建在数据库的唯一键上——
// 前端的按钮禁用挡不住刷新重发。
//
// 同 key 二次调用返回 domain.CodeConflict。这里不去"返回已有的那条"，
// 因为接口注释明确要求 409：让管理员看到"这次点击没有生效"，
// 比静默返回一条看起来成功的旧记录更不容易造成误操作。
func (l *ledger) Topup(ctx context.Context, userID string, amount int, reason, operatorID, idempotencyKey string) (domain.LedgerEntry, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if idempotencyKey == "" {
		return domain.LedgerEntry{}, invalidParam("topup requires an idempotency key")
	}
	if amount == 0 {
		return domain.LedgerEntry{}, invalidParam("topup amount must not be zero")
	}

	entry := domain.LedgerEntry{
		ID:             uid.New(),
		UserID:         userID,
		Type:           domain.LedgerTopup,
		Amount:         amount,
		Reason:         reason,
		IdempotencyKey: idempotencyKey,
	}
	if operatorID != "" {
		entry.OperatorID = &operatorID
	}

	err := withTx(ctx, l.db, func(tx *sql.Tx) error {
		// 充值不涉及冻结。
		_, err := appendLedgerTx(ctx, tx, &entry, 0)
		return err
	})
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	return entry, nil
}

// Balance 返回可用余额与冻结额度。
//
// 读的是 users 上的物化缓存而不是每次求和流水——那正是这两个列存在的理由。
// 怀疑漂移时走 store.LedgerRepo.Recompute 重算。
func (l *ledger) Balance(ctx context.Context, userID string) (domain.Balance, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.Balance{}, err
	}
	var b domain.Balance
	err := l.db.QueryRowContext(ctx,
		`SELECT credits, credits_held FROM users WHERE id = ?`, userID).Scan(&b.Available, &b.Held)
	if isNoRows(err) {
		return domain.Balance{}, notFound("user", userID)
	}
	if err != nil {
		return domain.Balance{}, wrap("read balance", err)
	}
	return b, nil
}

// taskHoldTx 读出某任务当前的冻结额，并报告它是否已被结算过。
//
// 事务内调用，依赖调用方已经通过 appendLedgerTx 之外的路径进入了事务；
// 这里的读不额外加锁，因为 appendLedgerTx 紧接着就会 FOR UPDATE 锁住用户行，
// 而同一用户的所有记账都串行在那把锁上。
func taskHoldTx(ctx context.Context, tx *sql.Tx, userID, taskID string) (held int, settled bool, err error) {
	const q = `SELECT
		  COALESCE(SUM(CASE WHEN type = 'hold' THEN -amount ELSE 0 END), 0),
		  SUM(type IN ('charge','refund'))
		FROM credit_ledger WHERE user_id = ? AND task_id = ?`
	var settledCount sql.NullInt64
	if err := tx.QueryRowContext(ctx, q, userID, taskID).Scan(&held, &settledCount); err != nil {
		return 0, false, wrap("read task hold", err)
	}
	if held < 0 {
		held = 0
	}
	return held, settledCount.Int64 > 0, nil
}
