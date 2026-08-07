package mysql

import (
	"context"
	"database/sql"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// ledgerRepo 是 store.LedgerRepo 的 MySQL 实现。
type ledgerRepo struct{ db *sql.DB }

// NewLedgerRepo 构造 store.LedgerRepo。
func NewLedgerRepo(db *sql.DB) store.LedgerRepo { return &ledgerRepo{db: db} }

const ledgerColumns = `id, user_id, task_id, type, amount, balance_after, reason,
	operator_id, idempotency_key, created_at`

func scanLedgerEntry(s rowScanner) (domain.LedgerEntry, error) {
	var (
		e          domain.LedgerEntry
		taskID     sql.NullString
		reason     sql.NullString
		operatorID sql.NullString
		idemKey    sql.NullString
	)
	err := s.Scan(
		&e.ID, &e.UserID, &taskID, &e.Type, &e.Amount, &e.BalanceAfter,
		&reason, &operatorID, &idemKey, &e.CreatedAt,
	)
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	e.TaskID = strPtr(taskID)
	if reason.Valid {
		e.Reason = reason.String
	}
	e.OperatorID = strPtr(operatorID)
	if idemKey.Valid {
		e.IdempotencyKey = idemKey.String
	}
	e.CreatedAt = e.CreatedAt.UTC()
	return e, nil
}

// Append 追加一条流水，并在**同一个事务里**更新 users.credits。
//
// 这是整个记账体系的地基：流水是唯一真相，users.credits 是它的物化缓存
// （见 internal/billing 的包注释）。两者分两次写，中间崩一次就会永久分叉，
// 而分叉出来的钱谁也说不清该以哪边为准。
//
// balance_after 由事务内锁住用户行后读到的余额算出，不信任调用方传进来的值：
// 并发的两笔扣费各自算出的 balance_after 都会是"我扣完之后"，
// 但只有串行化到数据库里的那个顺序才是真的。
func (r *ledgerRepo) Append(ctx context.Context, e domain.LedgerEntry) (domain.LedgerEntry, error) {
	if err := requireID("ledger user_id", e.UserID); err != nil {
		return domain.LedgerEntry{}, err
	}
	switch e.Type {
	case domain.LedgerHold, domain.LedgerCharge, domain.LedgerRefund,
		domain.LedgerTopup, domain.LedgerAdjust:
	default:
		return domain.LedgerEntry{}, invalidParam("unknown ledger entry type " + string(e.Type))
	}
	if e.ID == "" {
		e.ID = uid.New()
	}

	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		_, err := appendLedgerTx(ctx, tx, &e, defaultHeldDelta(e.Type, e.Amount))
		return err
	})
	if err != nil {
		return domain.LedgerEntry{}, err
	}
	return r.getByID(ctx, e.ID)
}

// appendLedgerTx 是 Append 的事务内实现，Ledger（billing 那一侧）复用它。
//
// 提出来是为了让 Hold / Charge / Refund / Topup 能在**一个**事务里
// 既写流水又改 users.credits / credits_held，而不是各自调一次 Append
// 再补一条 UPDATE——那样两条语句之间就有了一个能崩的缝。
//
// heldDelta 是本条流水对冻结额度的影响，**由调用方显式给出而不是从 amount 推**：
// charge 的 amount 是"退回多冻结的那部分"（hold − actual），它与该释放掉的
// 冻结额（整笔 hold）根本不是一个数——actual 与 hold 相等时 amount 为 0，
// 从它推出来的释放量就是 0，那笔冻结会永远挂在 credits_held 上，
// 用户每成功一个任务可用余额就凭空少一截。
func appendLedgerTx(ctx context.Context, tx *sql.Tx, e *domain.LedgerEntry, heldDelta int) (domain.Balance, error) {
	// 先锁住用户行。所有改余额的路径都从这一行开始，因此锁顺序天然一致，
	// 不会出现两个事务互相等对方的用户行。
	var balance domain.Balance
	err := tx.QueryRowContext(ctx,
		`SELECT credits, credits_held FROM users WHERE id = ? FOR UPDATE`,
		e.UserID).Scan(&balance.Available, &balance.Held)
	if isNoRows(err) {
		return domain.Balance{}, notFound("user", e.UserID)
	}
	if err != nil {
		return domain.Balance{}, wrap("lock user for ledger append", err)
	}

	// Credits 已经扣除了 Held，因此它就是可用余额（见 domain.User 的注释）。
	balance.Available += e.Amount
	balance.Held += heldDelta
	if balance.Held < 0 {
		// 冻结额度被释放得比冻结的多，说明上层重复退了款。压到 0 而不是
		// 留一个负数：负的冻结额度会让下一次余额检查算出一个虚高的可用额。
		balance.Held = 0
	}
	e.BalanceAfter = balance.Available

	_, err = tx.ExecContext(ctx,
		`INSERT INTO credit_ledger
		 (id, user_id, task_id, type, amount, balance_after, reason, operator_id, idempotency_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.UserID, nullString(e.TaskID), string(e.Type), e.Amount, e.BalanceAfter,
		nullStringNonEmpty(e.Reason), nullString(e.OperatorID),
		nullStringNonEmpty(e.IdempotencyKey))
	if err != nil {
		if isDup(err) {
			return domain.Balance{}, conflict(
				"ledger entry with idempotency key "+e.IdempotencyKey+" already exists", err)
		}
		return domain.Balance{}, wrap("append ledger entry", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET credits = ?, credits_held = ? WHERE id = ?`,
		balance.Available, balance.Held, e.UserID); err != nil {
		return domain.Balance{}, wrap("update credits cache", err)
	}
	return balance, nil
}

// defaultHeldDelta 是 store.LedgerRepo.Append 这条**通用**入口对冻结额度的估计。
//
//	hold   冻结额度增加（amount 为负，冻结量取其绝对值）
//	其余    不动冻结额度
//
// charge / refund 不在这里给出释放量：那需要知道该任务当初冻结了多少，
// 而通用的 Append 拿不到这个上下文。走 billing.Ledger 的 Charge / Refund
// 时由那一侧读出整笔 hold 再显式传进来（见 billing.go）。
func defaultHeldDelta(t domain.LedgerEntryType, amount int) int {
	if t == domain.LedgerHold {
		return -amount
	}
	return 0
}

func (r *ledgerRepo) getByID(ctx context.Context, id string) (domain.LedgerEntry, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+ledgerColumns+` FROM credit_ledger WHERE id = ?`, id)
	e, err := scanLedgerEntry(row)
	if err != nil {
		return domain.LedgerEntry{}, wrapScan("ledger entry", id, "get ledger entry", err)
	}
	return e, nil
}

// List 按 id DESC 游标分页，对齐 idx_credit_ledger_user_id (user_id, id DESC)。
//
// 这里只用 id 排序而不带时间：credit_ledger.id 是 UUIDv7，高 48 位就是毫秒
// 时间戳，因此 id 序即时间序，同毫秒内也由 id 给出稳定全序。迁移文件里
// "建议用 UUIDv7 以便游标分页才稳定"那句注释说的就是这件事。
func (r *ledgerRepo) List(ctx context.Context, userID, cursor string, limit int) (store.Page[domain.LedgerEntry], error) {
	if err := requireID("user id", userID); err != nil {
		return store.Page[domain.LedgerEntry]{}, err
	}
	limit = clampLimit(limit)

	where := []string{`user_id = ?`}
	args := []any{userID}
	if c, ok := decodeCursor(cursor); ok && c.ID != "" {
		where = append(where, `id < ?`)
		args = append(args, c.ID)
	}
	args = append(args, limit+1)

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+ledgerColumns+` FROM credit_ledger WHERE `+strings.Join(where, ` AND `)+
			` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return store.Page[domain.LedgerEntry]{}, wrap("list ledger entries", err)
	}
	defer rows.Close()

	var items []domain.LedgerEntry
	for rows.Next() {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return store.Page[domain.LedgerEntry]{}, wrap("scan ledger entry", err)
		}
		items = append(items, e)
	}
	if err := rows.Err(); err != nil {
		return store.Page[domain.LedgerEntry]{}, wrap("list ledger entries", err)
	}

	items, hasMore := pageSlice(items, limit)
	page := store.Page[domain.LedgerEntry]{Items: items}
	if hasMore && len(items) > 0 {
		page.NextCursor = encodeIDCursor(items[len(items)-1].ID)
	}
	return page, nil
}

// GetByIdempotencyKey 支撑管理员充值的防重复点击。
//
// 没找到返回 (零值, false, nil)：调用方问的是"这个 key 用过吗"，
// "没用过"是正常答案。
func (r *ledgerRepo) GetByIdempotencyKey(ctx context.Context, key string) (domain.LedgerEntry, bool, error) {
	if key == "" {
		return domain.LedgerEntry{}, false, nil
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+ledgerColumns+` FROM credit_ledger WHERE idempotency_key = ?`, key)
	e, err := scanLedgerEntry(row)
	if err != nil {
		if isNoRows(err) {
			return domain.LedgerEntry{}, false, nil
		}
		return domain.LedgerEntry{}, false, wrap("get ledger entry by idempotency key", err)
	}
	return e, true, nil
}

// Recompute 从流水重算余额，并把 users.credits 校正回真相。
//
// 余额是 SUM(amount)；冻结额度是"已 hold 但既没 charge 也没 refund"的那些
// 任务的冻结量之和。后者不能简单地对 hold 求和减去 charge/refund 求和——
// charge 的金额允许小于 hold（上游实际用量比预估少时少收），两者相减
// 剩下的差额并不是仍被冻结的钱。因此按 task_id 分组，只把"还没结算过"
// 的那些 hold 计入冻结。
//
// 全程在一个事务里：重算的意义就是让流水与缓存重新对上，读到一半被新流水
// 插进来会让"校正"本身写出一个新的错值。
func (r *ledgerRepo) Recompute(ctx context.Context, userID string) (domain.Balance, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.Balance{}, err
	}

	var balance domain.Balance
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var exists int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM users WHERE id = ? FOR UPDATE`, userID).Scan(&exists); {
		case isNoRows(err):
			return notFound("user", userID)
		case err != nil:
			return wrap("lock user for recompute", err)
		}

		var total sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(amount), 0) FROM credit_ledger WHERE user_id = ?`,
			userID).Scan(&total); err != nil {
			return wrap("recompute balance", err)
		}
		balance.Available = int(total.Int64)

		const heldQ = `SELECT COALESCE(SUM(-h.amount), 0)
			FROM credit_ledger h
			WHERE h.user_id = ? AND h.type = 'hold' AND h.task_id IS NOT NULL
			  AND NOT EXISTS (
			    SELECT 1 FROM credit_ledger s
			    WHERE s.user_id = h.user_id AND s.task_id = h.task_id
			      AND s.type IN ('charge','refund')
			  )`
		var held sql.NullInt64
		if err := tx.QueryRowContext(ctx, heldQ, userID).Scan(&held); err != nil {
			return wrap("recompute held", err)
		}
		balance.Held = int(held.Int64)
		if balance.Held < 0 {
			balance.Held = 0
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET credits = ?, credits_held = ? WHERE id = ?`,
			balance.Available, balance.Held, userID); err != nil {
			return wrap("write recomputed balance", err)
		}
		return nil
	})
	if err != nil {
		return domain.Balance{}, err
	}
	return balance, nil
}
