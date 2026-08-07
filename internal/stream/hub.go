package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultSubscriberBuffer 是单个订阅者的缓冲深度。
//
// 缓冲必须有界。一条 SSE 连接背后是一个可能已经断掉却还没被 TCP 发现的
// 浏览器标签页；无界 channel 在这种订阅者上会一直涨，最终把进程的内存吃光——
// 而这一切的起因只是有人合上了笔记本。
//
// 64 的量级来自实际节奏：任务事件是秒级的（轮询 12s 一次，加上进度更新），
// 64 条事件对应几分钟的积压。消费者只要还活着就绝不会碰到这个上限，
// 碰到了就说明它真的不在了。
const DefaultSubscriberBuffer = 64

// DefaultReplayLimit 是一次断线补发的事件条数上限。
//
// 补发只是第一道保险。断得足够久时该补的事件可能有几千条，把它们一次性
// 灌给前端既慢又没用——前端本来就要在重连后调 GET /api/tasks?status=active
// 做一次全量对账。因此补发只覆盖"短暂断线"这个场景，长断线交给对账。
const DefaultReplayLimit = 500

// ErrHubClosed 在 Hub 关闭后的订阅/发布上返回。
var ErrHubClosed = errors.New("stream: hub 已关闭")

// EventSink 是事件的持久化出口，由 store.EventRepo 满足。
//
// 这里重新声明一份最小接口而不是直接依赖 store：store 反过来 import 本包
// （EventRepo 的签名里就有 stream.Event），直接依赖会成环。
// 只声明本包真正要用的两个方法，也顺带说明了 Hub 到底碰了持久层的哪一小块。
type EventSink interface {
	// Append 分配单调递增 id 并落库，返回该 id。
	Append(ctx context.Context, ev Event) (int64, error)
	// ListAfter 取某用户 afterID 之后的事件，用于断线补发。
	ListAfter(ctx context.Context, userID string, afterID int64, limit int) ([]Event, error)
}

// HubDeps 是 Hub 的依赖。
type HubDeps struct {
	// Events 持久化事件并支撑补发。为 nil 时 Hub 退化成纯内存广播：
	// 事件仍会送达在线订阅者，但断线补发失效。生产必须注入。
	Events EventSink
	// Buffer 是单订阅者的缓冲深度，<=0 时取 DefaultSubscriberBuffer。
	Buffer int
	// ReplayLimit 是一次补发的条数上限，<=0 时取 DefaultReplayLimit。
	ReplayLimit int
	// PingEvery 是心跳间隔，<=0 时取 PingInterval。
	PingEvery time.Duration
	Logger    *slog.Logger
	Now       func() time.Time
}

// Broker 是 Hub 的默认实现，一个**单进程内**的扇出分发器。
//
// 多实例部署时，A 实例上的 worker 发的事件不会直接推到连在 B 实例上的浏览器——
// 那一跳靠的是持久化：事件已经落库，客户端重连（或全量对账接口）能拿到。
// 用一条广播总线跨实例分发需要引入消息中间件，而本项目的规模不值得为它
// 多一个必须运维的组件。
//
// 它是具体类型而不是接口，因为除了 Hub 声明的两个方法之外还要向进程的
// 停机序列暴露 Close——把 Close 塞进 Hub 接口会让每个只想发事件的调用方
// 都拿到关闭整条事件总线的能力。
type Broker struct {
	events      EventSink
	buffer      int
	replayLimit int
	pingEvery   time.Duration
	log         *slog.Logger
	now         func() time.Time

	mu     sync.Mutex
	subs   map[string]map[*subscriber]struct{}
	closed bool

	// memSeq 在没有 EventSink 时充当 id 分配器，保证 Event.ID 在
	// 纯内存模式下也单调递增——前端把它当 Last-Event-ID 用，不能是 0。
	memSeq int64
}

// subscriber 是一条活跃订阅。
//
// 它带自己的锁而不是复用 Hub 的锁：投递与关闭必须互斥（否则会往一个
// 刚被关掉的 channel 上发送，那是 panic 而不是错误），但这个互斥的范围
// 只该是这一条订阅——用 Hub 的锁会让一个用户的投递挡住所有其他用户的。
type subscriber struct {
	ch     chan Event
	userID string

	mu     sync.Mutex
	closed bool
	// dropped 记录因缓冲满而丢弃的事件数，只在诊断日志里用。
	dropped int
}

// send 非阻塞投递一条事件，缓冲满或订阅已关时返回 false。
func (s *subscriber) send(ev Event) (ok bool, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, s.dropped
	}
	select {
	case s.ch <- ev:
		return true, s.dropped
	default:
		s.dropped++
		return false, s.dropped
	}
}

// close 关闭订阅的 channel。约定由实现关、调用方只读，见 Hub 的注释。
// 可重复调用：ctx 结束与 Broker.Close 可能同时发生。
func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// NewHub 组装一个事件中心。
func NewHub(d HubDeps) *Broker {
	h := &Broker{
		events:      d.Events,
		buffer:      d.Buffer,
		replayLimit: d.ReplayLimit,
		pingEvery:   d.PingEvery,
		log:         d.Logger,
		now:         d.Now,
		subs:        make(map[string]map[*subscriber]struct{}),
	}
	if h.buffer <= 0 {
		h.buffer = DefaultSubscriberBuffer
	}
	if h.replayLimit <= 0 {
		h.replayLimit = DefaultReplayLimit
	}
	if h.pingEvery <= 0 {
		h.pingEvery = PingInterval
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Subscribe 为某用户开一条订阅。
//
// 补发先于实时推送写入 channel，因此客户端看到的 id 序列始终是升序的。
// 补发期间新到的实时事件不会丢：订阅在补发**之前**就已经注册进 subs，
// 那期间发布的事件进的是同一个 channel 的队尾。代价是极小概率的重复
// （一条事件既在补发范围内又被实时投递），而 SSE 客户端本就按 id 去重——
// 重复一条远好过漏一条。
func (h *Broker) Subscribe(ctx context.Context, userID string, lastEventID int64) (<-chan Event, error) {
	if userID == "" {
		return nil, fmt.Errorf("stream: 订阅缺少 user id")
	}
	if ctx == nil {
		return nil, fmt.Errorf("stream: 订阅缺少 context")
	}

	sub := &subscriber{ch: make(chan Event, h.buffer), userID: userID}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrHubClosed
	}
	if h.subs[userID] == nil {
		h.subs[userID] = make(map[*subscriber]struct{})
	}
	h.subs[userID][sub] = struct{}{}
	h.mu.Unlock()

	// ctx 结束时摘掉订阅并关 channel。调用方只读不关，见 Hub 的注释。
	go func() {
		<-ctx.Done()
		h.remove(sub)
	}()

	if lastEventID > 0 && h.events != nil {
		past, err := h.events.ListAfter(ctx, userID, lastEventID, h.replayLimit)
		if err != nil {
			// 补发失败不该让连接建不起来：前端还有全量对账那条路，
			// 而连不上 SSE 会让它连实时更新都拿不到。
			h.log.Warn("断线补发失败，客户端将依赖全量对账", "user_id", userID, "last_event_id", lastEventID, "err", err)
		} else {
			for _, ev := range past {
				if !h.offer(sub, ev) {
					h.log.Warn("补发时订阅缓冲已满，剩余事件交给全量对账", "user_id", userID)
					break
				}
			}
		}
	}

	// 心跳。它穿透的是代理的空闲连接超时——没有它，一条几分钟没有事件的
	// 连接会被中间层悄悄掐掉，而客户端要到下一次写才发现。
	go h.pingLoop(ctx, sub)

	return sub.ch, nil
}

// Publish 分配 id、持久化，然后投递给该用户当前全部活跃订阅。
//
// **没有活跃订阅时也必须成功返回。** 用户可能正好在刷新页面，事件落库后
// 等他重连补发即可。发布路径一旦能因为"没人在听"而失败或阻塞，
// 一条无人订阅的事件就会把调用它的那个任务状态机卡死。
func (h *Broker) Publish(ctx context.Context, ev Event) error {
	if ev.UserID == "" {
		return fmt.Errorf("stream: 发布事件缺少 user id")
	}
	if ev.Type == "" {
		return fmt.Errorf("stream: 发布事件缺少类型")
	}

	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return ErrHubClosed
	}

	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = h.now().UTC()
	}

	if h.events != nil {
		id, err := h.events.Append(ctx, ev)
		if err != nil {
			// 落库失败必须报错：id 是补发的唯一依据，发一条没有 id 的事件
			// 等于在客户端的 Last-Event-ID 序列上开一个它永远发现不了的洞。
			return fmt.Errorf("stream: 持久化事件: %w", err)
		}
		ev.ID = id
	} else {
		h.mu.Lock()
		h.memSeq++
		ev.ID = h.memSeq
		h.mu.Unlock()
	}

	h.fanout(ev)
	return nil
}

// Close 关闭 Hub 并断开全部订阅。进程停机时调用。
func (h *Broker) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	all := make([]*subscriber, 0)
	for _, set := range h.subs {
		for s := range set {
			all = append(all, s)
		}
	}
	h.subs = make(map[string]map[*subscriber]struct{})
	h.mu.Unlock()

	for _, s := range all {
		s.close()
	}
	return nil
}

// fanout 把事件投给该用户的全部订阅。
//
// 同一个用户开两个标签页时两边都要收到，因此是遍历而不是取一个。
// 跨用户绝不投递——一条连接只收自己的事件，越权投递等于数据泄露。
func (h *Broker) fanout(ev Event) {
	h.mu.Lock()
	targets := make([]*subscriber, 0, len(h.subs[ev.UserID]))
	for s := range h.subs[ev.UserID] {
		targets = append(targets, s)
	}
	h.mu.Unlock()

	for _, s := range targets {
		if ok, dropped := s.send(ev); !ok {
			h.log.Warn("订阅缓冲已满，丢弃事件",
				"user_id", ev.UserID, "type", ev.Type, "event_id", ev.ID, "dropped", dropped)
		}
	}
}

// offer 尝试非阻塞投递，缓冲满时丢弃并返回 false。
//
// **发布方绝不等待消费者。** 一个卡住的浏览器不该让 worker 的状态机跟着停——
// 那会把一个客户端的问题放大成全站的问题。丢掉的事件由客户端重连时的
// 补发或全量对账兜底，这正是 Event.ID 单调递增的用处。
func (h *Broker) offer(s *subscriber, ev Event) bool {
	ok, _ := s.send(ev)
	return ok
}

// pingLoop 每 pingEvery 发一条 ping，直到 ctx 结束。
//
// ping 不落库、不占 id：它是传输层的保活，不是业务事件。给它分配 id 会让
// Last-Event-ID 的序列里混进一堆客户端根本不关心的空洞。
func (h *Broker) pingLoop(ctx context.Context, s *subscriber) {
	t := time.NewTicker(h.pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ping := Event{
				UserID:    s.userID,
				Type:      EventPing,
				Data:      struct{}{},
				CreatedAt: h.now().UTC(),
			}
			// ping 投不进去说明消费者已经堵死了，此时更不该阻塞在它上面。
			h.offer(s, ping)
		}
	}
}

// remove 摘掉一条订阅并关闭它的 channel。
func (h *Broker) remove(s *subscriber) {
	h.mu.Lock()
	if set, ok := h.subs[s.userID]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(h.subs, s.userID)
		}
	}
	h.mu.Unlock()
	s.close()
}
