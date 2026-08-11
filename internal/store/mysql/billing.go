package mysql

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

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
			// 串行重复调用的快路径。并发时它会漏（快照读），最终由 settleTx
			// 的唯一键顶掉——两条路都得回同一个哨兵，调用方才分得清。
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
		return settleTx(ctx, tx, &entry, -held)
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
			// 这里是**串行**重复调用的快路径：判定在快照里做，真并发时它会
			// 放行，由 settleTx 的唯一键做最终裁决（见 taskHoldTx 的注释）。
			//
			// cause 带上 billing.ErrAlreadySettled，是为了让调用方能把这一种
			// conflict 与"真的没退成"分开——两者的 Code 都是 conflict。
			return conflict("task "+taskID+" has already been settled", billing.ErrAlreadySettled)
		}
		entry.Amount = held
		return settleTx(ctx, tx, &entry, -held)
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
//
// 键由管理员自己给，而任务结算也写这一列（见 settleIdempotencyKey），
// 因此带结算前缀的键必须拒收：那等于让人手工把某个任务的结算钉死。
func (l *ledger) Topup(ctx context.Context, userID string, amount int, reason, operatorID, idempotencyKey string) (domain.LedgerEntry, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.LedgerEntry{}, err
	}
	if idempotencyKey == "" {
		return domain.LedgerEntry{}, invalidParam("topup requires an idempotency key")
	}
	if strings.HasPrefix(idempotencyKey, settleKeyPrefix) {
		return domain.LedgerEntry{}, invalidParam(
			"topup idempotency key must not start with " + settleKeyPrefix +
				"; that namespace belongs to task settlement")
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

// settleKeyPrefix 是结算流水在 credit_ledger.idempotency_key 上占用的命名空间。
//
// 这一列被管理员充值与任务结算共用同一个唯一键，因此必须分开命名空间：
// 管理员若能提交一个 "settle:<task-id>" 形态的充值键，就能把那个任务的
// 结算永久顶掉——冻结额再也释放不了，用户的钱卡在 credits_held 上。
// Topup 因此拒收带本前缀的键。
const settleKeyPrefix = "settle:"

// settleIdempotencyKey 是一个任务的结算幂等键。
//
// 每个任务只允许结算一次，charge 与 refund **共用同一个键**：它们是同一件事
// 的两种结果（收钱 / 不收钱），不是两笔可以并存的账。这与 taskHoldTx 里
// "type IN ('charge','refund') 即已结算"的定义是同一条规则，
// 只是换成了数据库能强制执行的形式。
//
// 长度：前缀 7 + task_id 36 = 43，装得进 VARCHAR(64)。
func settleIdempotencyKey(taskID string) string { return settleKeyPrefix + taskID }

// settleTx 落一条结算流水（charge / refund），并让唯一键充当幂等闸。
//
// taskHoldTx 的 settled 判定在 REPEATABLE READ 的快照里做，并发时两笔结算
// 都会被放行（见 taskHoldTx 的注释）。这里给流水打上确定性的
// settleIdempotencyKey，把最终裁决交给 uq_credit_ledger_idempotency：
// 先提交的那笔留下，晚到的那笔 INSERT 撞 1062 后整个事务回滚，
// 一分钱都没动。
//
// 选唯一键而不是给 taskHoldTx 那条 SELECT 加 FOR UPDATE，是因为后者会
// 引入一个新的加锁顺序：Hold 是"先锁 users 行、再写 credit_ledger"，而加锁读
// credit_ledger 会让结算变成"先锁 credit_ledger（且空结果集会退化成
// idx_credit_ledger_task 上的间隙锁）、再锁 users 行"。两条路径的顺序反了，
// 同一用户上并发的 Hold 与 Refund 就能凑出死锁——DEM-96 刚为消掉一个
// S→X 升级死锁调过语句顺序，不值得再换一个回来。唯一键这条路不新增任何锁：
// idempotency_key 所在的索引本来就要因这次 INSERT 被写一遍。
//
// 1062 在这条路径上只可能来自 settleIdempotencyKey——它是本次 INSERT 写进
// 唯一键的唯一值，因此可以确定地翻成 billing.ErrAlreadySettled，
// 让调用方（publish.go / runner.go / chatbill.go）继续把它与"真的没写进去"分开。
func settleTx(ctx context.Context, tx *sql.Tx, entry *domain.LedgerEntry, heldDelta int) error {
	taskID := *entry.TaskID
	entry.IdempotencyKey = settleIdempotencyKey(taskID)
	if _, err := appendLedgerTx(ctx, tx, entry, heldDelta); err != nil {
		if isDup(err) {
			return conflict("task "+taskID+" has already been settled", billing.ErrAlreadySettled)
		}
		return err
	}
	return nil
}

// taskHoldTx 读出某任务当前的冻结额，并报告它是否已被结算过。
//
// **这个 settled 判定只是快路径，不是幂等闸本身。** 事务隔离级别是
// REPEATABLE READ，本函数这条 SELECT 往往就是事务的第一次读，一致性快照
// 由它钉死；因此并发的两笔结算各自看到的都是"还没结算"，谁也拦不住谁。
// 之后 appendLedgerTx 的 `SELECT ... FOR UPDATE` 只把两笔**写**串行化，
// 并不能让已经做完的**判定**重新生效——历史上这里的注释把这两件事
// 混为一谈，代价是并发取消一次任务能退两笔钱（DEM-101）。
//
// 真正的闸在 credit_ledger.idempotency_key 的唯一键上：结算类流水写
// settleIdempotencyKey(taskID)，第二笔在 INSERT 时撞 1062 被顶掉
// （见 settleTx）。因此这里读到的 settled 只用来在**串行**重复调用时
// 省掉一次必然失败的写，它给出 false 是安全的。
//
// held 同样来自快照。它只在"本任务的 hold 已提交"这个前提下有意义，
// 而 hold 必然发生在任务入队之前、远早于任何结算。
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
