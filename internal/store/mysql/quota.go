package mysql

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/aigc-pool/aigc-pool/internal/asset"
)

// quotaGuard 是 asset.QuotaGuard 的 MySQL 实现。
//
// 预占记在 users.storage_used_bytes 这一个列上：预占不单独开一张表，
// 因为"预占中"与"已占用"对配额检查的效果完全一样（都占着空间），
// 而多一张表就多一份要对账的状态。真相仍然是 assets 表逐条求和，
// Recompute 据此校正漂移。
//
// **这个列上谁写什么，是一条必须守住的分工**：已落库资产的字节由
// AssetRepo.Create / Delete 在写 assets 行的同一个事务里加减，本类型只负责
// 那些**还没有 assets 行的字节**（即预占）。两边都往同一个列上加实际字节，
// 结果就是一次转存记两遍（DEM-98：全库 ratio 精确 2.000）。
type quotaGuard struct{ db *sql.DB }

// NewQuotaGuard 构造 asset.QuotaGuard。
func NewQuotaGuard(db *sql.DB) asset.QuotaGuard { return &quotaGuard{db: db} }

// Reserve 预占额度，超限返回 domain.CodeQuotaExceeded。
//
// 检查点在**转存之前**：产物一旦下载落盘字节就已经占掉了，那时再发现超配额
// 只能删掉重来，白白浪费一次上游生成的钱和一次带宽（见 QuotaGuard 的注释）。
//
// 检查与占用在一个事务里，理由与积分的 Hold 完全相同：不锁行就检查，
// 并发转存的每一个都会看到"还有空间"，最后一起超配额。
func (g *quotaGuard) Reserve(ctx context.Context, userID string, bytes int64) error {
	if err := requireID("user id", userID); err != nil {
		return err
	}
	if bytes < 0 {
		return invalidParam("reserve bytes must not be negative")
	}
	if bytes == 0 {
		return nil
	}

	return withTx(ctx, g.db, func(tx *sql.Tx) error {
		var used, quota int64
		switch err := tx.QueryRowContext(ctx,
			`SELECT storage_used_bytes, storage_quota_bytes FROM users WHERE id = ? FOR UPDATE`,
			userID).Scan(&used, &quota); {
		case isNoRows(err):
			return notFound("user", userID)
		case err != nil:
			return wrap("lock user for reserve", err)
		}

		if used+bytes > quota {
			return quotaExceeded(
				"storage quota exceeded: " + strconv.FormatInt(used+bytes, 10) +
					" bytes would exceed the quota of " + strconv.FormatInt(quota, 10))
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET storage_used_bytes = storage_used_bytes + ? WHERE id = ?`,
			bytes, userID); err != nil {
			return wrap("reserve storage", err)
		}
		return nil
	})
}

// Commit 在资产行落库之后撤掉预占。
//
// **它不记实际字节，只撤预占。** 实际字节由 AssetRepo.Create 在写 assets 行的
// 同一个事务里记进这个列，Commit 再按"实际 − 预占"补一次差额的话，一次转存的
// 净效果就是 预占 + 实际 + (实际 − 预占) = 2 × 实际——这正是 DEM-98 里
// 全库比值精确等于 2.000 的来源。
//
// 归属点选在 AssetRepo 而不是这里，理由是这个列的**定义**就是"未软删资产的
// 字节之和"（Recompute 正是照这个定义把它重写回去），因此只有改变那个和的
// 代码才有资格永久改它。预占代表的是**还没有 assets 行的字节**（见 Recompute
// 的注释），行一旦落库，这些字节就已经由行本身代表，预占必须同时让位，
// 否则同一份字节被表示了两次。
//
// 与 Release 保持两个方法而不合并：撤预占的算术相同，但调用点的意图不同
// （Commit 之后有一件已落库的资产，Release 之后没有），合并会让"成功结算"与
// "失败回滚"在日志和断言里再也分不开。
//
// **实际超出预占时不再检查配额**：字节已经落盘了，这时报 quota_exceeded
// 只会让一个本已成功的转存变成失败，产物白拿。超出的部分由 Create 记进用量，
// 下一次 Reserve 自然会被挡住——那才是应该拒绝的时刻。
func (g *quotaGuard) Commit(ctx context.Context, userID string, reserved int64) error {
	if err := requireID("user id", userID); err != nil {
		return err
	}
	if reserved < 0 {
		return invalidParam("commit bytes must not be negative")
	}
	if reserved == 0 {
		return nil
	}
	// GREATEST(0, ...) 兜住历史漂移，与 Release 同理：撤预占不该把缓存压成负数，
	// 负用量会让配额检查永远通过。
	//
	// 不再断言"影响行数非零"：钳到 0 时新值可能与旧值相同，MySQL 会报 0 行受
	// 影响，据此判"用户不存在"会误报。用户是否存在由 Reserve 的 FOR UPDATE 查过。
	if _, err := g.db.ExecContext(ctx,
		`UPDATE users SET storage_used_bytes = GREATEST(0, storage_used_bytes - ?) WHERE id = ?`,
		reserved, userID); err != nil {
		return wrap("commit storage", err)
	}
	return nil
}

// Release 在转存失败时释放预占，避免失败任务把配额慢慢吃光。
func (g *quotaGuard) Release(ctx context.Context, userID string, bytes int64) error {
	if err := requireID("user id", userID); err != nil {
		return err
	}
	if bytes < 0 {
		return invalidParam("release bytes must not be negative")
	}
	if bytes == 0 {
		return nil
	}
	_, err := g.db.ExecContext(ctx,
		`UPDATE users SET storage_used_bytes = GREATEST(0, storage_used_bytes - ?) WHERE id = ?`,
		bytes, userID)
	if err != nil {
		return wrap("release storage", err)
	}
	return nil
}

// Usage 读缓存里的用量。
func (g *quotaGuard) Usage(ctx context.Context, userID string) (asset.Usage, error) {
	if err := requireID("user id", userID); err != nil {
		return asset.Usage{}, err
	}
	const q = `SELECT u.storage_used_bytes, u.storage_quota_bytes,
		  (SELECT COUNT(*) FROM assets a WHERE a.user_id = u.id AND a.deleted_at IS NULL)
		FROM users u WHERE u.id = ?`
	var usage asset.Usage
	err := g.db.QueryRowContext(ctx, q, userID).Scan(&usage.UsedBytes, &usage.QuotaBytes, &usage.AssetCount)
	if isNoRows(err) {
		return asset.Usage{}, notFound("user", userID)
	}
	if err != nil {
		return asset.Usage{}, wrap("read storage usage", err)
	}
	return usage, nil
}

// Recompute 从 assets 表重算真实用量，修正因崩溃等原因漂移的缓存值。
//
// 与积分同理：累加出来的用量是缓存，逐条求和才是真相。
//
// **重算结果会把预占中的字节抹掉**——那些字节还没有对应的 assets 行。
// 这是可接受的：正在转存的那一件最多几十到几百 MB，而重算解决的是
// 崩溃遗留下来的、可能大到让用户完全存不进新东西的漂移。
// 因此这个方法只该由清理任务在低峰调用，不该挂在读路径上。
func (g *quotaGuard) Recompute(ctx context.Context, userID string) (asset.Usage, error) {
	if err := requireID("user id", userID); err != nil {
		return asset.Usage{}, err
	}

	var usage asset.Usage
	err := withTx(ctx, g.db, func(tx *sql.Tx) error {
		switch err := tx.QueryRowContext(ctx,
			`SELECT storage_quota_bytes FROM users WHERE id = ? FOR UPDATE`,
			userID).Scan(&usage.QuotaBytes); {
		case isNoRows(err):
			return notFound("user", userID)
		case err != nil:
			return wrap("lock user for recompute", err)
		}

		var used sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(bytes), 0), COUNT(*) FROM assets
			 WHERE user_id = ? AND deleted_at IS NULL`,
			userID).Scan(&used, &usage.AssetCount); err != nil {
			return wrap("recompute storage usage", err)
		}
		usage.UsedBytes = used.Int64

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET storage_used_bytes = ? WHERE id = ?`, usage.UsedBytes, userID); err != nil {
			return wrap("write recomputed storage usage", err)
		}
		return nil
	})
	if err != nil {
		return asset.Usage{}, err
	}
	return usage, nil
}
