package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/executor"
)

// queue 是 executor.Queue 的 MySQL 实现。
//
// 租约用数据库行做，不引入额外中间件：tasks 表上已经有 lease_owner 与
// lease_expires_at 两列，而"少一个要运维的组件"在这个规模上比"更专业的
// 队列"更值钱。租约过期是进程崩溃后任务能自愈的**全部**机制——
// 崩掉的 worker 不会来续租，到点后别的 worker 就能接手。
type queue struct{ db *sql.DB }

// NewQueue 构造 executor.Queue。
func NewQueue(db *sql.DB) executor.Queue { return &queue{db: db} }

// defaultLeaseTTL 是 ttl <= 0 时的兜底。租约太短会让正常执行中的任务被
// 重复领取，太长会让崩溃恢复变慢；一分钟够绝大多数同步族任务跑完，
// 视频这类长任务靠 Renew 续。
const defaultLeaseTTL = time.Minute

// leaseDeadline 算出租约到期时刻，并**截到毫秒**。
//
// lease_expires_at 是 DATETIME(3)，而驱动会把 time.Time 按微秒发出去。
// 不截断的话，写进去的值被库四舍五入成毫秒，紧接着按同一个变量
// `WHERE lease_expires_at = ?` 读回就匹配不上——Claim 会在明明已经打上租约的
// 情况下报告"没领到任务"，而那条任务的租约还挂着，要等它过期才能再被领。
// 这条 bug 只在毫秒以下的位不为零时出现，也就是几乎每一次。
func leaseDeadline(ttl time.Duration) time.Time {
	return time.Now().UTC().Add(ttl).Truncate(time.Millisecond)
}

// Claim 原子地领取一条 queued 任务并打上租约。
//
// 一条 UPDATE 完成"挑一条 + 打标记"，再按 owner 读回：
// 这样两个 worker 同时抢时，数据库保证只有一个能把行改成自己的 owner。
// 先 SELECT 再 UPDATE 会在两条语句之间留出让另一个 worker 抢到同一行的窗口。
//
// providerIDs 限定只领这几家供应商的任务，是故障隔离的一部分：熔断打开的
// 供应商不出现在这个列表里，它的任务就留在队列里等熔断恢复，
// 而不是被领出来一个个超时把 worker 占满。
//
// **没有可领的任务时返回 (Lease{}, false, nil)，这不是错误。** 队列空是常态，
// 让它变成一个 error 会让 worker 的主循环被淹没在无意义的错误日志里。
func (q *queue) Claim(ctx context.Context, owner string, ttl time.Duration, providerIDs []string) (executor.Lease, bool, error) {
	if owner == "" {
		return executor.Lease{}, false, invalidParam("lease owner must not be empty")
	}
	// providerIDs 为空表示"没有任何一家供应商可用"（全部熔断），
	// 而不是"不限供应商"：后者会在熔断全开时把任务照领不误，
	// 正好抵消掉故障隔离。
	if len(providerIDs) == 0 {
		return executor.Lease{}, false, nil
	}
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(providerIDs)), ",")
	expires := leaseDeadline(ttl)

	// ORDER BY created_at ASC：先提交的先跑，队列语义。
	// lease_expires_at < NOW(3) 那一支就是崩溃恢复：上一个 owner 没来续租，
	// 任务自动回到可领取状态。
	query := `UPDATE tasks SET lease_owner = ?, lease_expires_at = ?
		WHERE status = 'queued'
		  AND provider_id IN (` + placeholders + `)
		  AND (lease_expires_at IS NULL OR lease_expires_at < NOW(3))
		ORDER BY created_at ASC
		LIMIT 1`

	args := make([]any, 0, len(providerIDs)+2)
	args = append(args, owner, expires)
	for _, id := range providerIDs {
		args = append(args, id)
	}

	res, err := q.db.ExecContext(ctx, query, args...)
	if err != nil {
		return executor.Lease{}, false, wrap("claim task", err)
	}
	n, err := affected(res, "claim task")
	if err != nil {
		return executor.Lease{}, false, err
	}
	if n == 0 {
		return executor.Lease{}, false, nil
	}

	// 读回刚打上的那一行。按 (owner, expires) 定位：owner 里带随机后缀，
	// 同一个 owner 在同一毫秒内只可能有这一次 Claim 写出这个组合。
	leases, err := q.readLeases(ctx, owner, expires, `t.status = 'queued'`, 1)
	if err != nil {
		return executor.Lease{}, false, err
	}
	if len(leases) == 0 {
		// 抢到了租约但读回时行已经不是 queued——被取消了。
		// 这不是错误，让 worker 直接进下一轮。
		return executor.Lease{}, false, nil
	}
	return leases[0], true, nil
}

// ClaimPoll 领取一批到点该轮询的 running 任务（next_poll_at <= now），同样打租约。
//
// 与 Claim 分开，是因为两者扫的是不同的索引（这里走
// idx_tasks_status_next_poll），也不该互相饿死：一个 worker 池被大量待轮询
// 任务占满时，新提交的任务仍要能被领走。
func (q *queue) ClaimPoll(ctx context.Context, owner string, ttl time.Duration, limit int) ([]executor.Lease, error) {
	if owner == "" {
		return nil, invalidParam("lease owner must not be empty")
	}
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	limit = clampLimit(limit)
	expires := leaseDeadline(ttl)

	const query = `UPDATE tasks SET lease_owner = ?, lease_expires_at = ?
		WHERE status = 'running'
		  AND next_poll_at IS NOT NULL AND next_poll_at <= NOW(3)
		  AND (lease_expires_at IS NULL OR lease_expires_at < NOW(3))
		ORDER BY next_poll_at ASC
		LIMIT ?`

	res, err := q.db.ExecContext(ctx, query, owner, expires, limit)
	if err != nil {
		return nil, wrap("claim poll tasks", err)
	}
	n, err := affected(res, "claim poll tasks")
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return q.readLeases(ctx, owner, expires, `t.status = 'running'`, int(n))
}

// readLeases 读回被某次 Claim/ClaimPoll 打上标记的那些行。
//
// statusCond 是调用方给的**常量**片段（本文件里只有两个取值），
// 不接受任何外部输入拼进来。
func (q *queue) readLeases(ctx context.Context, owner string, expires time.Time, statusCond string, limit int) ([]executor.Lease, error) {
	query := `SELECT ` + taskColumns + taskFrom + `
		WHERE t.lease_owner = ? AND t.lease_expires_at = ? AND ` + statusCond + `
		LIMIT ?`

	rows, err := q.db.QueryContext(ctx, query, owner, expires, limit)
	if err != nil {
		return nil, wrap("read leases", err)
	}
	defer rows.Close()

	var out []executor.Lease
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, wrap("scan leased task", err)
		}
		out = append(out, executor.Lease{Task: t, Owner: owner, ExpiresAt: expires})
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("read leases", err)
	}
	return out, nil
}

// Renew 延长租约。
//
// 长任务（视频动辄几分钟）必须周期性调用它，否则租约过期会被另一个 worker
// 重复领取，同一个任务被提交两次到上游——那是真金白银的重复消费。
//
// WHERE 带 lease_owner = owner：只有当前持有者能续。租约已经被别人接管时
// 返回 409，让原持有者知道自己该停手，而不是继续往一个已经不归它管的任务上写。
func (q *queue) Renew(ctx context.Context, taskID, owner string, ttl time.Duration) error {
	if err := requireID("task id", taskID); err != nil {
		return err
	}
	if owner == "" {
		return invalidParam("lease owner must not be empty")
	}
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}

	res, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET lease_expires_at = ? WHERE id = ? AND lease_owner = ?`,
		leaseDeadline(ttl), taskID, owner)
	if err != nil {
		return wrap("renew lease", err)
	}
	n, err := affected(res, "renew lease")
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}

	var currentOwner sql.NullString
	switch err := q.db.QueryRowContext(ctx,
		`SELECT lease_owner FROM tasks WHERE id = ?`, taskID).Scan(&currentOwner); {
	case isNoRows(err):
		return notFound("task", taskID)
	case err != nil:
		return wrap("read lease owner", err)
	}
	if currentOwner.Valid && currentOwner.String == owner {
		// owner 没变，0 行只可能是新到期时刻与旧值逐位相同——续租成功。
		return nil
	}
	return conflict("lease on task "+taskID+" is no longer held by "+owner, nil)
}

// Release 主动放弃租约。
//
// 任务进终态时调用，或 worker 优雅退出时把在途任务立刻还回队列
// （不还也行，等租约过期，但那要多等一个租约周期）。
//
// 不是持有者时静默返回而不是报错：worker 退出时会对一批任务挨个 Release，
// 其中某几条的租约可能刚被接管，为此报错只会让优雅退出的日志变成一片红，
// 而"我不再持有它"这个结果本来就是 Release 想要的。
func (q *queue) Release(ctx context.Context, taskID, owner string) error {
	if err := requireID("task id", taskID); err != nil {
		return err
	}
	if owner == "" {
		return invalidParam("lease owner must not be empty")
	}
	_, err := q.db.ExecContext(ctx,
		`UPDATE tasks SET lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND lease_owner = ?`, taskID, owner)
	if err != nil {
		return wrap("release lease", err)
	}
	return nil
}
