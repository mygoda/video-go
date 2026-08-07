package stream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memEvents 是 EventSink 的内存实现，分配单调递增 id 并保存历史供补发。
type memEvents struct {
	mu     sync.Mutex
	next   int64
	events []Event
	// failAppend 用来验证"落库失败不能把状态机卡住"。
	failAppend error
	// failList 用来验证补发失败只降级、不断连接。
	failList error
}

func (m *memEvents) Append(_ context.Context, ev Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAppend != nil {
		return 0, m.failAppend
	}
	m.next++
	ev.ID = m.next
	m.events = append(m.events, ev)
	return m.next, nil
}

func (m *memEvents) ListAfter(_ context.Context, userID string, afterID int64, limit int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failList != nil {
		return nil, m.failList
	}
	out := make([]Event, 0, limit)
	for _, ev := range m.events {
		if ev.UserID == userID && ev.ID > afterID {
			out = append(out, ev)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memEvents) seed(userID string, n int) {
	for i := 0; i < n; i++ {
		_, _ = m.Append(context.Background(), Event{
			UserID: userID,
			Type:   EventTaskUpdated,
			Data:   TaskUpdatedPayload{TaskID: "t"},
		})
	}
}

func testHub(t *testing.T, ev *memEvents, mut func(*HubDeps)) *Broker {
	t.Helper()
	d := HubDeps{
		Events: ev,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// 心跳设得远一点，免得干扰断言；单独的用例专门测它。
		PingEvery: time.Hour,
	}
	if mut != nil {
		mut(&d)
	}
	return NewHub(d)
}

// recv 在超时内读一条事件。
func recv(t *testing.T, ch <-chan Event, within time.Duration) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(within):
		return Event{}, false
	}
}

// TestHubFanOutToSameUser 验证同一用户开两个标签页时两边都收到。
func TestHubFanOutToSameUser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := &memEvents{}
	h := testHub(t, ev, nil)
	defer h.Close()

	a, err := h.Subscribe(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("Subscribe a: %v", err)
	}
	b, err := h.Subscribe(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("Subscribe b: %v", err)
	}

	if err := h.Publish(ctx, Event{UserID: "u1", Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t1"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for name, ch := range map[string]<-chan Event{"a": a, "b": b} {
		got, ok := recv(t, ch, time.Second)
		if !ok {
			t.Fatalf("订阅 %s 没有收到事件", name)
		}
		if got.ID != 1 {
			t.Fatalf("订阅 %s 收到的 id 应为 1，实际 %d", name, got.ID)
		}
	}
}

// TestHubDoesNotLeakAcrossUsers 验证一条连接只收自己的事件。
// 越权投递等于数据泄露。
func TestHubDoesNotLeakAcrossUsers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := testHub(t, &memEvents{}, nil)
	defer h.Close()

	mine, _ := h.Subscribe(ctx, "u1", 0)
	_, _ = h.Subscribe(ctx, "u2", 0)

	if err := h.Publish(ctx, Event{UserID: "u2", Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, ok := recv(t, mine, 200*time.Millisecond); ok {
		t.Fatal("收到了别的用户的事件")
	}
}

// TestHubReplaysFromLastEventID 验证重连补发。
func TestHubReplaysFromLastEventID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := &memEvents{}
	ev.seed("u1", 5)
	h := testHub(t, ev, nil)
	defer h.Close()

	ch, err := h.Subscribe(ctx, "u1", 2)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for want := int64(3); want <= 5; want++ {
		got, ok := recv(t, ch, time.Second)
		if !ok {
			t.Fatalf("应补发 id=%d", want)
		}
		if got.ID != want {
			t.Fatalf("补发顺序错：want %d got %d", want, got.ID)
		}
	}

	// 补发完转入实时。
	_ = h.Publish(ctx, Event{UserID: "u1", Type: EventTaskSucceeded, Data: TaskSucceededPayload{TaskID: "t"}})
	got, ok := recv(t, ch, time.Second)
	if !ok || got.ID != 6 {
		t.Fatalf("补发后应转入实时推送，got %+v ok=%v", got, ok)
	}
}

// TestHubLastEventIDZeroDoesNotReplay 验证 lastEventID=0 时只收新事件。
func TestHubLastEventIDZeroDoesNotReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := &memEvents{}
	ev.seed("u1", 3)
	h := testHub(t, ev, nil)
	defer h.Close()

	ch, _ := h.Subscribe(ctx, "u1", 0)
	if _, ok := recv(t, ch, 200*time.Millisecond); ok {
		t.Fatal("lastEventID=0 不该补发历史")
	}
}

// TestHubReplayFailureDoesNotBreakSubscription 验证补发失败只降级。
//
// 补发只是第一道保险，前端重连后还会调 /api/tasks?status=active 做全量对账；
// 为了一次补发失败就断掉连接，等于把实时推送也一并砍了。
func TestHubReplayFailureDoesNotBreakSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := &memEvents{failList: errors.New("库挂了")}
	h := testHub(t, ev, nil)
	defer h.Close()

	ch, err := h.Subscribe(ctx, "u1", 1)
	if err != nil {
		t.Fatalf("补发失败不该让 Subscribe 失败：%v", err)
	}
	ev.mu.Lock()
	ev.failList = nil
	ev.mu.Unlock()

	_ = h.Publish(ctx, Event{UserID: "u1", Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t"}})
	if _, ok := recv(t, ch, time.Second); !ok {
		t.Fatal("补发失败后实时推送仍应可用")
	}
}

// TestPublishNeverBlocksOnSlowConsumer 是这个 Hub 最要紧的一条性质。
//
// 一个卡住不读的订阅者（浏览器标签页被挂起、网络黑洞）绝不能让 Publish 阻塞——
// Publish 在任务状态机的路径上，阻塞它等于让一个前端连接把后端的生成流程卡死。
func TestPublishNeverBlocksOnSlowConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := testHub(t, &memEvents{}, func(d *HubDeps) { d.Buffer = 4 })
	defer h.Close()

	// 订阅但一个字节都不读。
	if _, err := h.Subscribe(ctx, "u1", 0); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			if err := h.Publish(ctx, Event{UserID: "u1", Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t"}}); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish 被慢消费者阻塞住了")
	}
}

// TestPublishSucceedsWithNoSubscribers 验证没人订阅时发布照样成功。
//
// 用户可能正好断线，事件落库后等他重连时补发。发布路径绝不能因为
// "没人在听"而失败，否则一条没人订阅的事件会把整个任务状态机卡住。
func TestPublishSucceedsWithNoSubscribers(t *testing.T) {
	ev := &memEvents{}
	h := testHub(t, ev, nil)
	defer h.Close()

	if err := h.Publish(context.Background(), Event{UserID: "nobody", Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t"}}); err != nil {
		t.Fatalf("无订阅者时 Publish 应成功：%v", err)
	}
	ev.mu.Lock()
	n := len(ev.events)
	ev.mu.Unlock()
	if n != 1 {
		t.Fatalf("事件仍应落库供之后补发，实际存了 %d 条", n)
	}
}

// TestSubscriptionClosesOnContextCancel 验证 ctx 结束时由实现关闭 channel。
func TestSubscriptionClosesOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := testHub(t, &memEvents{}, nil)
	defer h.Close()

	ch, err := h.Subscribe(ctx, "u1", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // 已关闭，符合预期
			}
		case <-deadline:
			t.Fatal("ctx 取消后 channel 应被实现关闭")
		}
	}
}

// TestHubCloseClosesAllSubscriptions 验证停机时所有订阅被关掉。
func TestHubCloseClosesAllSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := testHub(t, &memEvents{}, nil)
	ch, _ := h.Subscribe(ctx, "u1", 0)

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("Close 后订阅应被关闭")
		}
	}
closed:
	if err := h.Publish(context.Background(), Event{UserID: "u1", Type: EventTaskUpdated}); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("关闭后 Publish 应返回 ErrHubClosed，实际 %v", err)
	}
	if _, err := h.Subscribe(ctx, "u1", 0); !errors.Is(err, ErrHubClosed) {
		t.Fatalf("关闭后 Subscribe 应返回 ErrHubClosed，实际 %v", err)
	}
}

// TestHubPings 验证心跳按节奏发出，且不占用 event id。
//
// ping 是穿透代理空闲超时用的，它不是业务事件；给它分配 id 会让客户端的
// Last-Event-ID 前进到一个补发时查不到对应内容的位置。
func TestHubPings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ev := &memEvents{}
	h := testHub(t, ev, func(d *HubDeps) { d.PingEvery = 20 * time.Millisecond })
	defer h.Close()

	ch, _ := h.Subscribe(ctx, "u1", 0)
	got, ok := recv(t, ch, 2*time.Second)
	if !ok {
		t.Fatal("应收到心跳")
	}
	if got.Type != EventPing {
		t.Fatalf("应为 ping，实际 %s", got.Type)
	}
	if got.ID != 0 {
		t.Fatalf("ping 不该占用 event id，实际 %d", got.ID)
	}

	ev.mu.Lock()
	n := len(ev.events)
	ev.mu.Unlock()
	if n != 0 {
		t.Fatalf("ping 不该落库，实际存了 %d 条", n)
	}
}

// TestHubConcurrentPublishSubscribe 在 -race 下压一遍并发路径。
func TestHubConcurrentPublishSubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := testHub(t, &memEvents{}, func(d *HubDeps) { d.Buffer = 8 })
	defer h.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sctx, scancel := context.WithCancel(ctx)
			defer scancel()
			ch, err := h.Subscribe(sctx, "u1", 0)
			if err != nil {
				return
			}
			for j := 0; j < 20; j++ {
				select {
				case <-ch:
				case <-time.After(50 * time.Millisecond):
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = h.Publish(ctx, Event{UserID: "u1", Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t"}})
			}
		}()
	}
	wg.Wait()
}

// ── SSE 线格式 ────────────────────────────────────────────────────────

// TestWriteHeaders 验证三条"做错了实时性直接归零"的响应头。
func TestWriteHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	WriteHeaders(w)
	h := w.Header()

	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := h.Get(k); !strings.Contains(got, want) {
			t.Errorf("%s = %q, 应包含 %q", k, got, want)
		}
	}
	// 任何一层压缩缓冲都会让事件成批到达。
	if enc := h.Get("Content-Encoding"); enc != "" && enc != "identity" {
		t.Errorf("不该开压缩，Content-Encoding = %q", enc)
	}
}

// TestParseLastEventID 验证只认请求头、不认 query。
//
// token 与游标进 URL 会进日志和 Referer；这里连游标也一并走头部，
// 保持"SSE 的状态只在头里"这一条规则没有例外。
func TestParseLastEventID(t *testing.T) {
	cases := []struct {
		header string
		query  string
		want   int64
	}{
		{header: "42", want: 42},
		{header: "", want: 0},
		{header: "abc", want: 0},
		{header: "-5", want: 0},
		{header: "", query: "?last_event_id=99", want: 0},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/events"+c.query, nil)
		if c.header != "" {
			r.Header.Set("Last-Event-ID", c.header)
		}
		if got := ParseLastEventID(r); got != c.want {
			t.Errorf("header=%q query=%q → %d, want %d", c.header, c.query, got, c.want)
		}
	}
}

// TestEncode 验证 SSE 帧格式。
func TestEncode(t *testing.T) {
	var sb strings.Builder
	ev := Event{ID: 7, Type: EventTaskUpdated, Data: TaskUpdatedPayload{TaskID: "t1", Status: "running"}}
	if err := Encode(&sb, ev); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := sb.String()

	for _, want := range []string{"event: task.updated\n", "id: 7\n", "data: "} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("帧必须以空行结束：%q", out)
	}

	// ping 不带 id，客户端的 Last-Event-ID 因此不会被心跳推进。
	sb.Reset()
	if err := Encode(&sb, Event{Type: EventPing}); err != nil {
		t.Fatalf("Encode ping: %v", err)
	}
	if strings.Contains(sb.String(), "id:") {
		t.Errorf("ping 不该带 id：%q", sb.String())
	}
}
