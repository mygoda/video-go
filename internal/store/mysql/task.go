package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// taskRepo 是 store.TaskRepo 的 MySQL 实现。
type taskRepo struct{ db *sql.DB }

// NewTaskRepo 构造 store.TaskRepo。
func NewTaskRepo(db *sql.DB) store.TaskRepo { return &taskRepo{db: db} }

// taskColumns 是任务的完整投影。
//
// model_name 与 modality 从 models join 过来而不是冗余进 tasks：这两个字段
// 只在展示时用到，冗余存一份就要面对"管理员改了模型名，历史任务显示哪个"
// 这个没有好答案的问题。JOIN 走的是 models 的主键，代价可以忽略。
const taskColumns = `t.id, t.user_id, t.model_id, m.name, m.modality, t.status,
	t.progress, t.queue_position, t.eta_seconds,
	t.prompt, t.params, t.inputs,
	t.estimated_cost, t.actual_cost,
	t.error_code, t.error_message, t.error_field_errors,
	t.canvas_id, t.card_id, t.client_token,
	t.provider_id, t.attempt, t.max_attempts, t.upstream_ref, t.upstream_status_raw,
	t.created_at, t.started_at, t.finished_at`

const taskFrom = ` FROM tasks t JOIN models m ON m.id = t.model_id`

func scanTask(s rowScanner) (domain.Task, error) {
	var (
		t                 domain.Task
		progress          sql.NullFloat64
		queuePosition     sql.NullInt64
		etaSeconds        sql.NullInt64
		prompt            sql.NullString
		paramsRaw         []byte
		inputsRaw         []byte
		actualCost        sql.NullInt64
		errorCode         sql.NullString
		errorMessage      sql.NullString
		errorFieldsRaw    []byte
		canvasID          sql.NullString
		cardID            sql.NullString
		upstreamRef       sql.NullString
		upstreamStatusRaw sql.NullString
		maxAttempts       int
		startedAt         sql.NullTime
		finishedAt        sql.NullTime
	)
	err := s.Scan(
		&t.ID, &t.UserID, &t.ModelID, &t.ModelName, &t.Modality, &t.Status,
		&progress, &queuePosition, &etaSeconds,
		&prompt, &paramsRaw, &inputsRaw,
		&t.EstimatedCost, &actualCost,
		&errorCode, &errorMessage, &errorFieldsRaw,
		&canvasID, &cardID, &t.ClientToken,
		&t.ProviderID, &t.Attempt, &maxAttempts, &upstreamRef, &upstreamStatusRaw,
		&t.CreatedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		return domain.Task{}, err
	}

	t.Progress = floatPtr(progress)
	t.QueuePosition = intPtr(queuePosition)
	if etaSeconds.Valid {
		v := float64(etaSeconds.Int64)
		t.ETASeconds = &v
	}
	if prompt.Valid {
		t.Prompt = prompt.String
	}
	if err := scanJSON(paramsRaw, &t.Params); err != nil {
		return domain.Task{}, err
	}
	if err := scanJSON(inputsRaw, &t.Inputs); err != nil {
		return domain.Task{}, err
	}
	t.ActualCost = intPtr(actualCost)
	t.CanvasID = strPtr(canvasID)
	t.CardID = strPtr(cardID)
	t.UpstreamRef = strPtr(upstreamRef)
	t.UpstreamStatusRaw = strPtr(upstreamStatusRaw)
	t.CreatedAt = t.CreatedAt.UTC()
	t.StartedAt = timePtr(startedAt)
	t.FinishedAt = timePtr(finishedAt)

	if errorCode.Valid {
		terr := &domain.TaskError{
			Code:    domain.TaskErrorCode(errorCode.String),
			Message: errorMessage.String,
			// 失败三分类恒不扣费，见 internal/billing 的包注释。Charged 从库里
			// 重建时永远是 false：能写进 error_code 的任务就是没拿到产物的任务。
			Charged: false,
		}
		if err := scanJSON(errorFieldsRaw, &terr.FieldErrors); err != nil {
			return domain.Task{}, err
		}
		// 限流是唯一会自动重试的失败类，RetryInfo 由 attempt / max_attempts 重建；
		// 其他失败类不展示重试进度，给了反而误导。
		if terr.Code == domain.TaskErrorUpstreamRateLimited {
			terr.Retryable = t.Attempt < maxAttempts
			terr.Retry = &domain.RetryInfo{Attempt: t.Attempt, MaxAttempts: maxAttempts}
		}
		t.Error = terr
	}
	return t, nil
}

func (r *taskRepo) Create(ctx context.Context, t domain.Task) (domain.Task, error) {
	if err := requireID("task user_id", t.UserID); err != nil {
		return domain.Task{}, err
	}
	if err := requireID("task model_id", t.ModelID); err != nil {
		return domain.Task{}, err
	}
	if err := requireID("task provider_id", t.ProviderID); err != nil {
		return domain.Task{}, err
	}
	if t.ID == "" {
		t.ID = uid.New()
	}
	if t.Status == "" {
		t.Status = domain.TaskStatusQueued
	}
	// client_token 是 NOT NULL 且参与 (user_id, client_token) 唯一键。调用方
	// 没给时生成一个，而不是落空串——空串会让同一用户的第二个无 token 任务
	// 撞上唯一键，把"没传幂等键"变成一个莫名其妙的 409。
	if t.ClientToken == "" {
		t.ClientToken = uid.Token(16)
	}

	params, err := jsonValue(t.Params)
	if err != nil {
		return domain.Task{}, invalidParam("task params is not valid JSON: " + err.Error())
	}
	inputs, err := jsonValue(t.Inputs)
	if err != nil {
		return domain.Task{}, invalidParam("task inputs is not valid JSON: " + err.Error())
	}

	const q = `INSERT INTO tasks
		(id, user_id, model_id, provider_id, canvas_id, card_id, client_token, status,
		 prompt, params, inputs, estimated_cost, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	createdAt := t.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	if _, err := r.db.ExecContext(ctx, q,
		t.ID, t.UserID, t.ModelID, t.ProviderID,
		nullString(t.CanvasID), nullString(t.CardID), t.ClientToken, t.Status,
		nullStringNonEmpty(t.Prompt), params, inputs, t.EstimatedCost, createdAt.UTC()); err != nil {
		if isDup(err) {
			return domain.Task{}, conflict("client_token "+t.ClientToken+" already used by this user", err)
		}
		return domain.Task{}, wrap("create task", err)
	}
	return r.Get(ctx, t.ID)
}

func (r *taskRepo) Get(ctx context.Context, id string) (domain.Task, error) {
	if err := requireID("task id", id); err != nil {
		return domain.Task{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+taskColumns+taskFrom+` WHERE t.id = ?`, id)
	t, err := scanTask(row)
	if err != nil {
		return domain.Task{}, wrapScan("task", id, "get task", err)
	}
	return t, nil
}

// GetByClientToken 是幂等提交的实现基础：同一用户重复提交同一个 token
// 直接返回已存在的任务（200 而非 201），断网重发不产生两个任务、不重复扣费。
//
// 没找到返回 (零值, false, nil) 而不是 not_found 错误：调用方问的是
// "这个 token 提交过吗"，"没提交过"是正常答案而不是异常。
func (r *taskRepo) GetByClientToken(ctx context.Context, userID, clientToken string) (domain.Task, bool, error) {
	if err := requireID("user id", userID); err != nil {
		return domain.Task{}, false, err
	}
	if clientToken == "" {
		return domain.Task{}, false, nil
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+taskFrom+` WHERE t.user_id = ? AND t.client_token = ?`,
		userID, clientToken)
	t, err := scanTask(row)
	if err != nil {
		if isNoRows(err) {
			return domain.Task{}, false, nil
		}
		return domain.Task{}, false, wrap("get task by client token", err)
	}
	return t, true, nil
}

// taskFilterSQL 把 TaskFilter 翻成 WHERE 片段与绑定参数。
//
// 每个分支都是**固定的 SQL 文本 + 占位符**，没有任何一处把调用方的值拼进
// 语句文本。状态列表这种个数不定的条件用循环生成同样固定的 "?" 序列，
// 值仍然全部走绑定——动态条数不等于动态 SQL。
func taskFilterSQL(f domain.TaskFilter) (where []string, args []any) {
	if f.UserID != "" {
		where = append(where, `t.user_id = ?`)
		args = append(args, f.UserID)
	}
	if len(f.Statuses) > 0 {
		placeholders := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			placeholders[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, `t.status IN (`+strings.Join(placeholders, ",")+`)`)
	}
	if f.ModelID != "" {
		where = append(where, `t.model_id = ?`)
		args = append(args, f.ModelID)
	}
	if f.CanvasID != "" {
		where = append(where, `t.canvas_id = ?`)
		args = append(args, f.CanvasID)
	}
	if f.ErrorCode != "" {
		where = append(where, `t.error_code = ?`)
		args = append(args, string(f.ErrorCode))
	}
	return where, args
}

// List 按 (created_at DESC, id DESC) 游标分页，对齐 idx_tasks_user_created。
func (r *taskRepo) List(ctx context.Context, f domain.TaskFilter) (store.Page[domain.Task], error) {
	limit := clampLimit(f.Limit)
	where, args := taskFilterSQL(f)

	if c, ok := decodeCursor(f.Cursor); ok {
		where = append(where, `(t.created_at < ? OR (t.created_at = ? AND t.id < ?))`)
		args = append(args, c.At, c.At, c.ID)
	}

	sqlText := `SELECT ` + taskColumns + taskFrom
	if len(where) > 0 {
		sqlText += ` WHERE ` + strings.Join(where, ` AND `)
	}
	sqlText += ` ORDER BY t.created_at DESC, t.id DESC LIMIT ?`
	args = append(args, limit+1)

	items, err := r.queryTasks(ctx, sqlText, args...)
	if err != nil {
		return store.Page[domain.Task]{}, err
	}

	items, hasMore := pageSlice(items, limit)
	page := store.Page[domain.Task]{Items: items}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		page.NextCursor = encodeTimeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ListActive 返回该用户全部未终结任务，**不分页**。
//
// 它是 SSE 重连后的对账接口：客户端断线期间漏掉的事件靠它补齐。
// 分页会让对账失去意义——第二页还没拉到时客户端的状态就是不完整的，
// 而它恰恰要在这一刻据此决定哪些任务还要继续等。
func (r *taskRepo) ListActive(ctx context.Context, userID string) ([]domain.Task, error) {
	if err := requireID("user id", userID); err != nil {
		return nil, err
	}
	const q = `SELECT ` + taskColumns + taskFrom + `
		WHERE t.user_id = ? AND t.status IN ('queued','running')
		ORDER BY t.created_at ASC, t.id ASC`
	return r.queryTasks(ctx, q, userID)
}

func (r *taskRepo) queryTasks(ctx context.Context, query string, args ...any) ([]domain.Task, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("query tasks", err)
	}
	defer rows.Close()

	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, wrap("scan task", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("query tasks", err)
	}
	return out, nil
}

// UpdateStatus 带前置状态的状态机推进，是幂等的底座。
//
// UPDATE ... WHERE status = expect 让轮询与 webhook 并发到达时只有一个能生效：
// 两者都在读到 running 后想写 succeeded，数据库保证只有先到的那条改动了行，
// 后到的看到 0 行受影响。**这个 0 行必须能被调用方分辨出来**，因此这里
// 区分了两种 0 行：行压根不存在是 404，行在但状态已经不是 expect 是 409。
// 混成一个错误会让调用方无法判断"我该重试还是该放弃"。
func (r *taskRepo) UpdateStatus(ctx context.Context, id string, expect, next domain.TaskStatus, terr *domain.TaskError) error {
	if err := requireID("task id", id); err != nil {
		return err
	}
	if expect == "" || next == "" {
		return invalidParam("task status transition requires both expect and next")
	}

	var fieldErrors any
	if terr != nil {
		var err error
		if fieldErrors, err = jsonValue(terr.FieldErrors); err != nil {
			return invalidParam("task error field_errors is not valid JSON: " + err.Error())
		}
	}

	now := time.Now().UTC()

	// started_at 在首次进入 running 时钉住（COALESCE 保证重试不会把它往后推，
	// 那样算出来的耗时会漏掉前几次尝试）；finished_at 只在进终态时写。
	// 进终态同时清掉租约与下次轮询时刻——终态任务不该再被任何 worker 领走，
	// 也不该继续占着 idx_tasks_status_next_poll 的扫描结果。
	const q = `UPDATE tasks SET
		  status              = ?,
		  error_code          = ?,
		  error_message       = ?,
		  error_field_errors  = ?,
		  started_at          = CASE WHEN ? = 'running' THEN COALESCE(started_at, ?) ELSE started_at END,
		  finished_at         = CASE WHEN ? IN ('succeeded','failed','canceled') THEN ? ELSE finished_at END,
		  next_poll_at        = CASE WHEN ? IN ('succeeded','failed','canceled') THEN NULL ELSE next_poll_at END,
		  lease_owner         = CASE WHEN ? IN ('succeeded','failed','canceled') THEN NULL ELSE lease_owner END,
		  lease_expires_at    = CASE WHEN ? IN ('succeeded','failed','canceled') THEN NULL ELSE lease_expires_at END
		WHERE id = ? AND status = ?`

	var errCode, errMessage any
	if terr != nil {
		errCode = string(terr.Code)
		errMessage = terr.Message
	}

	res, err := r.db.ExecContext(ctx, q,
		string(next), errCode, errMessage, fieldErrors,
		string(next), now,
		string(next), now,
		string(next), string(next), string(next),
		id, string(expect))
	if err != nil {
		return wrap("update task status", err)
	}
	n, err := affected(res, "update task status")
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}

	// 0 行：分辨"任务不存在"与"状态已经不是 expect"。
	var actual string
	switch err := r.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&actual); {
	case isNoRows(err):
		return notFound("task", id)
	case err != nil:
		return wrap("read task status", err)
	}
	// 幂等：目标状态已经达成（另一路先写成功了），当作成功返回。
	// 这是 webhook 与轮询同时到达时的常态，报错会让调用方误以为出了问题。
	if domain.TaskStatus(actual) == next {
		return nil
	}
	return conflict(fmt.Sprintf(
		"task %s is %s, expected %s", id, actual, expect), nil)
}

func (r *taskRepo) UpdateProgress(ctx context.Context, id string, progress *float64, queuePosition *int, etaSeconds *float64) error {
	if err := requireID("task id", id); err != nil {
		return err
	}
	// 只更新未终结的任务：一条迟到的进度推送不该把已经显示"完成"的卡片
	// 打回 87%。
	const q = `UPDATE tasks SET progress = ?, queue_position = ?, eta_seconds = ?
		WHERE id = ? AND status IN ('queued','running')`

	var eta any
	if etaSeconds != nil {
		eta = int(*etaSeconds)
	}
	if _, err := r.db.ExecContext(ctx, q,
		nullFloat(progress), nullInt(queuePosition), eta, id); err != nil {
		return wrap("update task progress", err)
	}
	return nil
}

// requireTaskExists 在一次影响 0 行的 UPDATE 之后，分辨"任务不存在"与
// "新值与旧值逐位相同"。
//
// 写上游引用、写实扣额度这类操作会被重复投递同样的值——webhook 与轮询读到的
// 是同一份上游响应——所以 0 行不能直接判 404，得再问一次这行在不在。
// 但也不能一律当成功：任务真的不存在时静默返回，会让执行侧以为自己记下了
// 实扣额度，而那笔钱其实哪儿都没写。
func requireTaskExists(ctx context.Context, db *sql.DB, id string) error {
	var exists int
	switch err := db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, id).Scan(&exists); {
	case isNoRows(err):
		return notFound("task", id)
	case err != nil:
		return wrap("check task exists", err)
	}
	return nil
}

func (r *taskRepo) SetUpstreamRef(ctx context.Context, id, upstreamRef, rawStatus string) error {
	if err := requireID("task id", id); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET upstream_ref = ?, upstream_status_raw = ? WHERE id = ?`,
		nullStringNonEmpty(upstreamRef), nullStringNonEmpty(rawStatus), id)
	if err != nil {
		return wrap("set upstream ref", err)
	}
	n, err := affected(res, "set upstream ref")
	if err != nil {
		return err
	}
	if n == 0 {
		return requireTaskExists(ctx, r.db, id)
	}
	return nil
}

// CountRunningByModel 统计该用户在某模型上未终结的任务数，
// 用于兑现 capability 里的 LimitSpec.MaxConcurrentPerUser。
func (r *taskRepo) CountRunningByModel(ctx context.Context, userID, modelID string) (int, error) {
	if err := requireID("user id", userID); err != nil {
		return 0, err
	}
	if err := requireID("model id", modelID); err != nil {
		return 0, err
	}
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks
		 WHERE user_id = ? AND model_id = ? AND status IN ('queued','running')`,
		userID, modelID).Scan(&n)
	if err != nil {
		return 0, wrap("count running tasks", err)
	}
	return n, nil
}

// SetNextPoll 排定下一次轮询时刻，PollScheduler 靠它驱动
// idx_tasks_status_next_poll 那条索引扫描。
func (r *taskRepo) SetNextPoll(ctx context.Context, id string, at time.Time) error {
	if err := requireID("task id", id); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET next_poll_at = ? WHERE id = ?`, at.UTC(), id)
	if err != nil {
		return wrap("set next poll", err)
	}
	n, err := affected(res, "set next poll")
	if err != nil {
		return err
	}
	if n == 0 {
		// 这里能安全地判 404：next_poll_at 每次都被写成一个新算出来的时刻，
		// 与旧值逐位相同的概率可以忽略。
		return notFound("task", id)
	}
	return nil
}

// IncrementAttempt 自增重试计数并返回新值。
//
// 自增与读回在同一事务里，否则两个 worker 同时重试时可能都读到同一个新值，
// 于是"第 3 次尝试"被记了两遍、重试耗尽的判定跟着失准。
func (r *taskRepo) IncrementAttempt(ctx context.Context, id string) (int, error) {
	if err := requireID("task id", id); err != nil {
		return 0, err
	}
	var attempt int
	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET attempt = attempt + 1 WHERE id = ?`, id)
		if err != nil {
			return wrap("increment attempt", err)
		}
		n, err := affected(res, "increment attempt")
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("task", id)
		}
		if err := tx.QueryRowContext(ctx, `SELECT attempt FROM tasks WHERE id = ?`, id).Scan(&attempt); err != nil {
			return wrapScan("task", id, "read attempt", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return attempt, nil
}

// SetActualCost 记录成功后的实扣额度。
func (r *taskRepo) SetActualCost(ctx context.Context, id string, actual int) error {
	if err := requireID("task id", id); err != nil {
		return err
	}
	if actual < 0 {
		return invalidParam("actual cost must not be negative")
	}
	res, err := r.db.ExecContext(ctx, `UPDATE tasks SET actual_cost = ? WHERE id = ?`, actual, id)
	if err != nil {
		return wrap("set actual cost", err)
	}
	n, err := affected(res, "set actual cost")
	if err != nil {
		return err
	}
	if n == 0 {
		return requireTaskExists(ctx, r.db, id)
	}
	return nil
}

// Requeue 把一条终态任务打回 queued（管理端手动重试）。
//
// **不动 estimated_cost、不重新冻结积分**：提交时那笔 hold 还在账上
// （失败退款走的是 Refund，管理员手动重试的是没退过款的那种情况），
// 重新冻结会在余额已被别的任务占满时把一条本可重试的任务卡死——
// 用户明明已经为这次生成付过冻结额度，却因为后来又提交了几个任务而重试不了。
//
// 同时清掉上一轮的执行痕迹（错误、租约、上游引用、完成时刻），只保留
// attempt 计数：那是"这条任务一共折腾了几次"的审计信息，清掉就看不出
// 一条任务是不是在反复失败。
func (r *taskRepo) Requeue(ctx context.Context, id string) error {
	if err := requireID("task id", id); err != nil {
		return err
	}
	const q = `UPDATE tasks SET
		  status              = 'queued',
		  error_code          = NULL,
		  error_message       = NULL,
		  error_field_errors  = NULL,
		  progress            = NULL,
		  queue_position      = NULL,
		  eta_seconds         = NULL,
		  upstream_ref        = NULL,
		  upstream_status_raw = NULL,
		  next_poll_at        = NULL,
		  lease_owner         = NULL,
		  lease_expires_at    = NULL,
		  started_at          = NULL,
		  finished_at         = NULL
		WHERE id = ? AND status IN ('succeeded','failed','canceled')`

	res, err := r.db.ExecContext(ctx, q, id)
	if err != nil {
		return wrap("requeue task", err)
	}
	n, err := affected(res, "requeue task")
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}

	var actual string
	switch err := r.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, id).Scan(&actual); {
	case isNoRows(err):
		return notFound("task", id)
	case err != nil:
		return wrap("read task status", err)
	}
	return conflict("task "+id+" is "+actual+" and is not in a terminal state", nil)
}

// Stats 汇总统计窗口内的任务表现，供管理端监控。
//
// window 为 0 时统计全量。label 原样回填进 TaskStats.Window——它是给人看的
// 窗口名（"24h"），由调用方决定怎么叫，仓储不去猜。
//
// Queued / Running 与 OldestQueuedAgeSeconds 刻意**不受窗口约束**：
// 它们回答的是"此刻积压多少、最老的那条等了多久"，把一条 3 天前卡住的任务
// 因为不在 24h 窗口内而排除掉，恰好漏掉了最该被看见的那一条。
func (r *taskRepo) Stats(ctx context.Context, window time.Duration, label string) (domain.TaskStats, error) {
	stats := domain.TaskStats{
		Window:      label,
		ByStatus:    map[domain.TaskStatus]int{},
		ByErrorCode: map[domain.TaskErrorCode]int{},
	}
	if stats.Window == "" && window > 0 {
		stats.Window = window.String()
	}

	// 窗口条件用一个恒真分支兜住"不限窗口"，这样 SQL 文本对两种情况完全一致，
	// 只是绑定参数不同——没有任何一处按条件拼接语句。
	var since time.Time
	unbounded := window <= 0
	if !unbounded {
		since = time.Now().UTC().Add(-window)
	}

	const statusQ = `SELECT status, COUNT(*) FROM tasks
		WHERE (? = 1 OR created_at >= ?) GROUP BY status`
	rows, err := r.db.QueryContext(ctx, statusQ, unbounded, since)
	if err != nil {
		return domain.TaskStats{}, wrap("task stats by status", err)
	}
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return domain.TaskStats{}, wrap("scan task stats", err)
		}
		stats.ByStatus[domain.TaskStatus(status)] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.TaskStats{}, wrap("task stats by status", err)
	}
	rows.Close()

	const errorQ = `SELECT error_code, COUNT(*) FROM tasks
		WHERE error_code IS NOT NULL AND (? = 1 OR created_at >= ?)
		GROUP BY error_code`
	rows, err = r.db.QueryContext(ctx, errorQ, unbounded, since)
	if err != nil {
		return domain.TaskStats{}, wrap("task stats by error code", err)
	}
	for rows.Next() {
		var (
			code string
			n    int
		)
		if err := rows.Scan(&code, &n); err != nil {
			rows.Close()
			return domain.TaskStats{}, wrap("scan task stats", err)
		}
		stats.ByErrorCode[domain.TaskErrorCode(code)] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.TaskStats{}, wrap("task stats by error code", err)
	}
	rows.Close()

	// 队列现状不看窗口，见方法注释。
	const liveQ = `SELECT
		  SUM(status = 'queued'),
		  SUM(status = 'running'),
		  MIN(CASE WHEN status = 'queued' THEN created_at END)
		FROM tasks`
	var (
		queued       sql.NullInt64
		running      sql.NullInt64
		oldestQueued sql.NullTime
	)
	if err := r.db.QueryRowContext(ctx, liveQ).Scan(&queued, &running, &oldestQueued); err != nil {
		return domain.TaskStats{}, wrap("task stats queue depth", err)
	}
	stats.Queued = int(queued.Int64)
	stats.Running = int(running.Int64)
	if oldestQueued.Valid {
		age := time.Since(oldestQueued.Time.UTC()).Seconds()
		if age < 0 {
			age = 0
		}
		stats.OldestQueuedAgeSeconds = &age
	}

	// 按模型的 p50 耗时。MySQL 8.4 没有开箱的百分位聚合，这里把窗口内每个
	// 模型的耗时全取回来在 Go 侧算——模型只有几十个、窗口内的已完成任务是
	// 千级，一次排序远比在 SQL 里手写窗口函数排名可读。
	const modelQ = `SELECT model_id, status,
		  CASE WHEN finished_at IS NOT NULL
		       THEN TIMESTAMPDIFF(MICROSECOND, created_at, finished_at) / 1000000
		  END
		FROM tasks WHERE (? = 1 OR created_at >= ?)`
	rows, err = r.db.QueryContext(ctx, modelQ, unbounded, since)
	if err != nil {
		return domain.TaskStats{}, wrap("task stats by model", err)
	}
	defer rows.Close()

	type modelAgg struct {
		total     int
		failed    int
		durations []float64
	}
	agg := map[string]*modelAgg{}
	var order []string
	for rows.Next() {
		var (
			modelID  string
			status   string
			duration sql.NullFloat64
		)
		if err := rows.Scan(&modelID, &status, &duration); err != nil {
			return domain.TaskStats{}, wrap("scan task stats", err)
		}
		a := agg[modelID]
		if a == nil {
			a = &modelAgg{}
			agg[modelID] = a
			order = append(order, modelID)
		}
		a.total++
		if domain.TaskStatus(status) == domain.TaskStatusFailed {
			a.failed++
		}
		if duration.Valid && domain.TaskStatus(status) == domain.TaskStatusSucceeded {
			a.durations = append(a.durations, duration.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		return domain.TaskStats{}, wrap("task stats by model", err)
	}

	sort.Strings(order)
	for _, modelID := range order {
		a := agg[modelID]
		stat := domain.TaskModelStat{ModelID: modelID, Total: a.total, Failed: a.failed}
		if p50, ok := percentile(a.durations, 0.5); ok {
			stat.P50DurationSeconds = &p50
		}
		stats.ByModel = append(stats.ByModel, stat)
	}
	return stats, nil
}

// percentile 返回排序后第 p 分位的值。样本为空时第二个返回值为 false——
// 那时给 0 会让管理端看到"这个模型 0 秒出图"。
func percentile(values []float64, p float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx], true
}
