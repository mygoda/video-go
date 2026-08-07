package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// eventRepo 是 store.EventRepo 的 MySQL 实现。
type eventRepo struct{ db *sql.DB }

// NewEventRepo 构造 store.EventRepo。
func NewEventRepo(db *sql.DB) store.EventRepo { return &eventRepo{db: db} }

// Append 落库一条事件并返回自增 id。
//
// id 由 AUTO_INCREMENT 分配而不是 uid.New()：它直接兼作 SSE 的 Last-Event-ID，
// 补发查的是"比这个大的"，UUID 排不出这种序（见 stream 包注释）。
//
// task_id 是可空外键，指向一条不存在的任务会被 InnoDB 拒掉（1452）。这里
// 把空串当 NULL 处理：credit.updated 这类事件本来就不属于任何任务。
func (r *eventRepo) Append(ctx context.Context, ev stream.Event) (int64, error) {
	if err := requireID("event user_id", ev.UserID); err != nil {
		return 0, err
	}
	if ev.Type == "" {
		return 0, invalidParam("event type must not be empty")
	}

	// payload 是 NOT NULL 列。Data 为 nil 时落一个空对象而不是 SQL NULL——
	// ping 事件本来就没有负载，它不该因此写不进去。
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return 0, invalidParam("event data is not valid JSON: " + err.Error())
	}
	if string(payload) == "null" {
		payload = []byte("{}")
	}

	createdAt := ev.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	res, err := r.db.ExecContext(ctx,
		`INSERT INTO task_events (user_id, task_id, event_type, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		ev.UserID, nullStringNonEmpty(taskIDOf(ev)), string(ev.Type), payload, createdAt.UTC())
	if err != nil {
		return 0, wrap("append event", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, wrap("append event", err)
	}
	return id, nil
}

// taskIDOf 从事件负载里挖出 task_id。
//
// stream.Event 上没有 TaskID 字段（它的 Data 才是按事件类型不同的负载），
// 但 task_events.task_id 这一列要能支撑"按任务查它的事件流"这条排障路径。
// 因此这里从负载里取——取不到就落 NULL，不是错误：credit.updated 与 ping
// 本来就不属于任何任务。
func taskIDOf(ev stream.Event) string {
	if ev.Data == nil {
		return ""
	}
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		return ""
	}
	var probe struct {
		TaskID string `json:"task_id"`
		Task   *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	if probe.TaskID != "" {
		return probe.TaskID
	}
	if probe.Task != nil {
		return probe.Task.ID
	}
	return ""
}

// ListAfter 补发某 id 之后的事件，对齐 idx_task_events_user_id。
//
// 按 id ASC 而不是 DESC：补发是按发生顺序重放，倒序会让客户端先看到
// task.succeeded 再看到 task.updated，进度条直接倒着走。
func (r *eventRepo) ListAfter(ctx context.Context, userID string, afterID int64, limit int) ([]stream.Event, error) {
	if err := requireID("user id", userID); err != nil {
		return nil, err
	}
	limit = clampLimit(limit)

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, event_type, payload, created_at
		 FROM task_events
		 WHERE user_id = ? AND id > ?
		 ORDER BY id ASC LIMIT ?`,
		userID, afterID, limit)
	if err != nil {
		return nil, wrap("list events", err)
	}
	defer rows.Close()

	var out []stream.Event
	for rows.Next() {
		var (
			ev      stream.Event
			evType  string
			payload []byte
		)
		if err := rows.Scan(&ev.ID, &ev.UserID, &evType, &payload, &ev.CreatedAt); err != nil {
			return nil, wrap("scan event", err)
		}
		ev.Type = stream.EventType(evType)
		ev.CreatedAt = ev.CreatedAt.UTC()
		if err := scanJSON(payload, &ev.Data); err != nil {
			return nil, wrap("decode event payload", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list events", err)
	}
	return out, nil
}

// DeleteBefore 清理过期事件，返回删除条数。
//
// 事件是补发用的短期缓冲而不是审计日志，无限堆积没有意义（见 EventRepo 注释）。
// 分批删而不是一条 DELETE 扫全表：一次删掉几十万行会长时间持有行锁，
// 把正在写事件的任务状态机一起卡住。
func (r *eventRepo) DeleteBefore(ctx context.Context, before time.Time) (int64, error) {
	const batch = 1000
	var total int64
	for {
		res, err := r.db.ExecContext(ctx,
			`DELETE FROM task_events WHERE created_at < ? LIMIT ?`, before.UTC(), batch)
		if err != nil {
			return total, wrap("delete events", err)
		}
		n, err := affected(res, "delete events")
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			return total, nil
		}
		// 每批之间让出一次，给并发的事件写入留出取锁的机会。
		select {
		case <-ctx.Done():
			return total, wrap("delete events", ctx.Err())
		default:
		}
	}
}
