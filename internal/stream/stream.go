// Package stream carries realtime events to connected clients over SSE.
//
// # 一条连接推该用户的全部任务
//
// 不是每个任务一条连接。用户同时跑 5 个任务不该开 5 条长连接——浏览器对
// 同源并发连接数有上限，几个任务就能把额度吃完，之后连普通 API 请求都发不出去。
//
// # 断线补发
//
// Event.ID 是**单调递增的 int64**，它同时就是 SSE 的 Last-Event-ID。
// 客户端重连时带上最后收到的 id，服务端补发其后的全部事件。
//
// 用递增整数而不是 UUID，是因为补发需要的是"比这个大的"这种可排序查询；
// UUID 做不到，就只能改用时间戳，而时间戳有精度碰撞和时钟回拨的问题。
//
// 补发只是第一道保险。SSE 断得足够久时事件可能已经被清理，因此前端在重连后
// 还要调 GET /api/tasks?status=active 做一次全量对账——那个接口不分页正是为此。
//
// # 传输层要求（做错了实时性直接归零）
//
//   - Cache-Control: no-cache
//   - X-Accel-Buffering: no
//   - **不开 gzip 缓冲**
//
// 任何一层缓冲都会让事件成批到达：用户盯着进度条不动，然后突然一次跳到完成。
// 每 15s 发一条 ping 穿透代理的空闲超时。
//
// 另外，原生 EventSource 不能带 Authorization 头，前端是用 fetch +
// ReadableStream 手写的解析，因此本端点**必须**接受 Bearer 头
// （不接受 query token —— token 进 URL 会进日志和 Referer）。
package stream

import (
	"context"
	"time"
)

// EventType 是事件类型，对应 SSE 的 event 字段。
type EventType string

const (
	EventTaskUpdated       EventType = "task.updated"
	EventTaskSucceeded     EventType = "task.succeeded"
	EventTaskFailed        EventType = "task.failed"
	EventCreditUpdated     EventType = "credit.updated"
	EventCanvasCardUpdated EventType = "canvas.card.updated"
	EventPing              EventType = "ping"
)

// PingInterval 是心跳间隔，用于穿透代理的空闲连接超时。
const PingInterval = 15 * time.Second

// Event 是一条待推送的事件。
//
// ID 单调递增，直接作为 SSE 的 id 字段下发，也就是客户端重连时回传的
// Last-Event-ID。UserID 决定投递给谁——一条连接只收自己的事件，
// 越权投递等于数据泄露。
//
// Data 是事件负载，按 Type 不同分别对应 openapi 的 TaskUpdatedEvent /
// TaskSucceededEvent / TaskFailedEvent / CreditUpdatedEvent /
// CanvasCardUpdatedEvent，ping 时为空对象。
type Event struct {
	ID        int64     `json:"-"`
	UserID    string    `json:"-"`
	Type      EventType `json:"-"`
	Data      any       `json:"data"`
	CreatedAt time.Time `json:"-"`
}

// Hub 是事件的发布订阅中心。
//
// Subscribe 为某用户开一条订阅。lastEventID 为客户端回传的 Last-Event-ID，
// 服务端先把该 id 之后的历史事件补发完，再转入实时推送；
// 传 0 表示不补发，只收新事件。
//
// 返回的 channel 在 ctx 结束时由实现关闭，调用方只读不关。
//
// Publish 发布一条事件。实现负责分配单调递增的 ID 并持久化（补发要靠它），
// 然后投递给该用户当前所有活跃订阅——同一个用户开两个标签页时两边都要收到。
//
// Publish 对没有活跃订阅的用户也必须正常返回：用户可能正好断线，
// 事件落库后等他重连时补发。发布路径绝不能因为"没人在听"而失败或阻塞，
// 否则一条没人订阅的事件会把整个任务状态机卡住。
type Hub interface {
	Subscribe(ctx context.Context, userID string, lastEventID int64) (<-chan Event, error)
	Publish(ctx context.Context, ev Event) error
}
