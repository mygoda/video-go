package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// TestTaskRepoCreateAndIdempotency 钉住幂等提交：同一 client_token 只有一条任务。
func TestTaskRepoCreateAndIdempotency(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewTaskRepo(db)
	f := newFixture(t, 100, 1<<30)

	token := "ct_" + uid.Token(10)
	created, err := repo.Create(ctx, domain.Task{
		UserID:        f.userID,
		ModelID:       f.modelID,
		ProviderID:    f.providerID,
		Prompt:        "hello",
		Params:        map[string]any{"aspect": "1:1"},
		Inputs:        map[string][]string{"image": {"up_1", "up_2"}},
		EstimatedCost: 7,
		ClientToken:   token,
	})
	requireNoErr(t, err, "create task")

	if created.Status != domain.TaskStatusQueued {
		t.Errorf("new task status = %q, want queued", created.Status)
	}
	if created.EstimatedCost != 7 {
		t.Errorf("estimated_cost not persisted: %d", created.EstimatedCost)
	}
	if created.ActualCost != nil {
		t.Errorf("actual_cost must be nil before settlement, got %v", *created.ActualCost)
	}
	// Inputs 是 map[string][]string，JSON 往返后必须还原成同样的形状而不是 []any。
	if got := created.Inputs["image"]; len(got) != 2 || got[0] != "up_1" || got[1] != "up_2" {
		t.Errorf("inputs lost in round trip: %+v", created.Inputs)
	}
	if created.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is not UTC: %v", created.CreatedAt)
	}

	// 断网重发：同 token 必须落到同一条任务上，否则用户被扣两次钱。
	got, ok, err := repo.GetByClientToken(ctx, f.userID, token)
	requireNoErr(t, err, "get by client token")
	if !ok {
		t.Fatal("client token lookup missed the task it was meant to dedupe")
	}
	if got.ID != created.ID {
		t.Errorf("client token resolved to %q, want %q", got.ID, created.ID)
	}

	// 换个用户问同一个 token：token 是按用户隔离的，不能串号。
	other := newUserNamed(t, "tok")
	if _, ok, err := repo.GetByClientToken(ctx, other.ID, token); err != nil || ok {
		t.Errorf("client token leaked across users: ok=%v err=%v", ok, err)
	}

	// 没用过的 token 是"没有"，不是错误。
	if _, ok, err := repo.GetByClientToken(ctx, f.userID, "ct_"+uid.Token(10)); err != nil || ok {
		t.Errorf("unused token should report (_, false, nil): ok=%v err=%v", ok, err)
	}

	requireCode(t, func() error { _, err := repo.Get(ctx, uid.New()); return err }(), domain.CodeNotFound)
}

// TestTaskRepoUpdateStatus 钉住状态机的三条规则：
// 前置状态不匹配是 409，已经是目标状态是幂等成功，任务不存在是 404。
func TestTaskRepoUpdateStatus(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewTaskRepo(db)
	f := newFixture(t, 100, 1<<30)
	task := f.newTask(t, 5)

	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil),
		"queued -> running")
	running, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if running.StartedAt == nil {
		t.Error("started_at must be stamped on the first transition into running")
	}
	if running.FinishedAt != nil {
		t.Error("finished_at must stay nil while the task is running")
	}
	startedAt := *running.StartedAt

	// 轮询与 webhook 同时判"还在跑"：第二次 queued->running 已经不成立，
	// 但目标状态已达成，按幂等成功处理而不是报错。
	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil),
		"idempotent re-transition into the state the task is already in")
	again, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if !again.StartedAt.Equal(startedAt) {
		t.Errorf("started_at moved on retry: %v -> %v", startedAt, *again.StartedAt)
	}

	// 前置状态不匹配且目标状态也没达成 → 409，调用方据此知道自己抢输了。
	requireCode(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusCanceled, nil),
		domain.CodeConflict)

	terr := &domain.TaskError{
		Code:        domain.TaskErrorContentRejected,
		Message:     "blocked by upstream safety filter",
		Retryable:   false,
		Charged:     false,
		FieldErrors: []domain.FieldError{{Key: "prompt", Message: "not allowed"}},
	}
	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusRunning, domain.TaskStatusFailed, terr),
		"running -> failed")

	failed, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if failed.Error == nil {
		t.Fatal("task error was not persisted")
	}
	if failed.Error.Code != domain.TaskErrorContentRejected {
		t.Errorf("error code = %q", failed.Error.Code)
	}
	if failed.Error.Charged {
		t.Error("the three failure classes must never report charged=true")
	}
	if len(failed.Error.FieldErrors) != 1 || failed.Error.FieldErrors[0].Key != "prompt" {
		t.Errorf("field_errors lost: %+v", failed.Error.FieldErrors)
	}
	if failed.FinishedAt == nil {
		t.Error("finished_at must be stamped when the task reaches a terminal state")
	}

	// 进终态要顺手清租约，否则终态任务还会被 worker 的 Claim 扫出来。
	var owner, expires any
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT lease_owner, lease_expires_at FROM tasks WHERE id = ?`, task.ID).Scan(&owner, &expires),
		"read lease columns")
	if owner != nil || expires != nil {
		t.Errorf("terminal task still carries a lease: owner=%v expires=%v", owner, expires)
	}

	// 迟到的进度推送不该把已完成的卡片打回进行中。
	p := 0.42
	requireNoErr(t, repo.UpdateProgress(ctx, task.ID, &p, nil, nil), "late progress update")
	after, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if after.Progress != nil {
		t.Errorf("progress was written onto a terminal task: %v", *after.Progress)
	}

	requireCode(t, repo.UpdateStatus(ctx, uid.New(), domain.TaskStatusQueued, domain.TaskStatusRunning, nil),
		domain.CodeNotFound)
}

// TestTaskRepoRequeue 钉住接口注释里那条：Requeue 不动 estimated_cost、不重新冻结积分。
func TestTaskRepoRequeue(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewTaskRepo(db)
	f := newFixture(t, 100, 1<<30)
	task := f.newTask(t, 9)

	// 走一遍真实的记账路径，好让 Requeue 之后能比对余额没被再冻一次。
	ledger := NewLedger(db)
	_, err := ledger.Hold(ctx, f.userID, task.ID, 9)
	requireNoErr(t, err, "hold credits")
	beforeBalance, err := ledger.Balance(ctx, f.userID)
	requireNoErr(t, err, "read balance")

	// 终态之前不能打回。
	requireCode(t, repo.Requeue(ctx, task.ID), domain.CodeConflict)

	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil), "to running")
	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusRunning, domain.TaskStatusFailed,
		&domain.TaskError{Code: domain.TaskErrorInternal, Message: "boom"}), "to failed")

	requireNoErr(t, repo.Requeue(ctx, task.ID), "requeue")
	back, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")

	if back.Status != domain.TaskStatusQueued {
		t.Errorf("requeued task status = %q, want queued", back.Status)
	}
	if back.EstimatedCost != 9 {
		t.Errorf("estimated_cost changed on requeue: %d, want 9", back.EstimatedCost)
	}
	if back.Error != nil {
		t.Errorf("stale error survived the requeue: %+v", back.Error)
	}
	if back.FinishedAt != nil || back.StartedAt != nil {
		t.Errorf("timestamps not cleared: started=%v finished=%v", back.StartedAt, back.FinishedAt)
	}

	// 重新冻结会在余额被别的任务占满时把一条本可重试的任务卡死。
	afterBalance, err := ledger.Balance(ctx, f.userID)
	requireNoErr(t, err, "read balance")
	if afterBalance != beforeBalance {
		t.Errorf("requeue moved credits: %+v -> %+v; the original hold must be reused",
			beforeBalance, afterBalance)
	}

	requireCode(t, repo.Requeue(ctx, uid.New()), domain.CodeNotFound)
}

// TestTaskRepoListAndActive 钉住列表分页与 ListActive 的不分页语义。
func TestTaskRepoListAndActive(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewTaskRepo(db)
	f := newFixture(t, 1000, 1<<30)

	const total = 5
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		ids = append(ids, f.newTask(t, 1).ID)
	}

	// 分页两页一取，不重不漏。
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		p, err := repo.List(ctx, domain.TaskFilter{UserID: f.userID, Cursor: cursor, Limit: 2})
		requireNoErr(t, err, "list tasks")
		for _, task := range p.Items {
			if seen[task.ID] {
				t.Fatalf("task %s appeared on two pages", task.ID)
			}
			seen[task.ID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged over %d tasks, want %d", len(seen), total)
	}

	// 垃圾游标降级成第一页而不是报错。
	p, err := repo.List(ctx, domain.TaskFilter{UserID: f.userID, Cursor: "!!!garbage!!!", Limit: 100})
	requireNoErr(t, err, "list with garbage cursor")
	if len(p.Items) != total {
		t.Errorf("garbage cursor returned %d tasks, want %d", len(p.Items), total)
	}

	// 把一条推进终态，它就该从 active 里消失。
	requireNoErr(t, repo.UpdateStatus(ctx, ids[0], domain.TaskStatusQueued, domain.TaskStatusCanceled, nil),
		"cancel one task")

	// ListActive 是 SSE 重连后的对账接口：必须一次给全，limit 不参与。
	active, err := repo.ListActive(ctx, f.userID)
	requireNoErr(t, err, "list active")
	if len(active) != total-1 {
		t.Fatalf("ListActive returned %d tasks, want all %d unfinished ones", len(active), total-1)
	}
	for _, task := range active {
		if task.Status.IsTerminal() {
			t.Errorf("terminal task %s leaked into ListActive", task.ID)
		}
	}

	// 按状态过滤。
	canceled, err := repo.List(ctx, domain.TaskFilter{
		UserID: f.userID, Statuses: []domain.TaskStatus{domain.TaskStatusCanceled}, Limit: 100,
	})
	requireNoErr(t, err, "list canceled")
	if len(canceled.Items) != 1 || canceled.Items[0].ID != ids[0] {
		t.Errorf("status filter returned %d tasks", len(canceled.Items))
	}

	// 按模型过滤：不存在的模型不该漏出任何任务。
	none, err := repo.List(ctx, domain.TaskFilter{UserID: f.userID, ModelID: "no-such-model", Limit: 100})
	requireNoErr(t, err, "list by unknown model")
	if len(none.Items) != 0 {
		t.Errorf("model filter matched %d tasks, want 0", len(none.Items))
	}
}

// TestTaskRepoExecutionFields 覆盖执行期的那几个窄接口。
func TestTaskRepoExecutionFields(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewTaskRepo(db)
	f := newFixture(t, 100, 1<<30)
	task := f.newTask(t, 3)

	requireNoErr(t, repo.SetUpstreamRef(ctx, task.ID, "upstream-abc", "IN_PROGRESS"), "set upstream ref")
	withRef, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if withRef.UpstreamRef == nil || *withRef.UpstreamRef != "upstream-abc" {
		t.Errorf("upstream_ref not persisted: %v", withRef.UpstreamRef)
	}
	if withRef.UpstreamStatusRaw == nil || *withRef.UpstreamStatusRaw != "IN_PROGRESS" {
		t.Errorf("upstream_status_raw not persisted: %v", withRef.UpstreamStatusRaw)
	}

	// IncrementAttempt 必须返回自增后的新值，重试耗尽判定全靠它。
	for want := 1; want <= 3; want++ {
		got, err := repo.IncrementAttempt(ctx, task.ID)
		requireNoErr(t, err, "increment attempt")
		if got != want {
			t.Fatalf("IncrementAttempt returned %d, want %d", got, want)
		}
	}

	at := time.Now().UTC().Add(30 * time.Second).Truncate(time.Millisecond)
	requireNoErr(t, repo.SetNextPoll(ctx, task.ID, at), "set next poll")
	var nextPoll time.Time
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT next_poll_at FROM tasks WHERE id = ?`, task.ID).Scan(&nextPoll), "read next_poll_at")
	if !nextPoll.Equal(at) {
		t.Errorf("next_poll_at = %v, want %v", nextPoll, at)
	}

	progress := 0.5
	queuePos := 3
	// eta_seconds 列是 INT，落库时被截断——秒以下的 ETA 精度对前端的
	// 乐观进度条没有意义，因此这里用整秒断言，把"会截断"这件事钉成已知行为
	// 而不是让一个 12.5 的期望在别处偶然失败。
	eta := 12.0
	requireNoErr(t, repo.UpdateProgress(ctx, task.ID, &progress, &queuePos, &eta), "update progress")
	withProgress, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if withProgress.Progress == nil || *withProgress.Progress != 0.5 {
		t.Errorf("progress not persisted: %v", withProgress.Progress)
	}
	if withProgress.QueuePosition == nil || *withProgress.QueuePosition != 3 {
		t.Errorf("queue_position not persisted: %v", withProgress.QueuePosition)
	}
	if withProgress.ETASeconds == nil || *withProgress.ETASeconds != 12 {
		t.Errorf("eta_seconds not persisted: %v", withProgress.ETASeconds)
	}

	requireNoErr(t, repo.SetActualCost(ctx, task.ID, 2), "set actual cost")
	costed, err := repo.Get(ctx, task.ID)
	requireNoErr(t, err, "re-read task")
	if costed.ActualCost == nil || *costed.ActualCost != 2 {
		t.Errorf("actual_cost not persisted: %v", costed.ActualCost)
	}

	// CountRunningByModel 数的是"未终结"（queued + running），不是只数 running：
	// 它兑现的是 LimitSpec.MaxConcurrentPerUser，而一条排队中的任务同样占着名额。
	n, err := repo.CountRunningByModel(ctx, f.userID, f.modelID)
	requireNoErr(t, err, "count running by model")
	if n != 1 {
		t.Errorf("queued task should count against the concurrency limit, got %d", n)
	}
	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil), "to running")
	n, err = repo.CountRunningByModel(ctx, f.userID, f.modelID)
	requireNoErr(t, err, "count running by model")
	if n != 1 {
		t.Errorf("running count = %d, want 1", n)
	}
	requireNoErr(t, repo.UpdateStatus(ctx, task.ID, domain.TaskStatusRunning, domain.TaskStatusSucceeded, nil), "to succeeded")
	n, err = repo.CountRunningByModel(ctx, f.userID, f.modelID)
	requireNoErr(t, err, "count running by model")
	if n != 0 {
		t.Errorf("terminal task still counts against the limit: %d", n)
	}

	requireCode(t, func() error { _, err := repo.IncrementAttempt(ctx, uid.New()); return err }(), domain.CodeNotFound)
	requireCode(t, repo.SetNextPoll(ctx, uid.New(), time.Now().UTC()), domain.CodeNotFound)

	// 写同一个值不算失败：webhook 与轮询读到的是同一份上游响应，重复投递是常态。
	requireNoErr(t, repo.SetUpstreamRef(ctx, task.ID, "upstream-abc", "IN_PROGRESS"), "repeat set upstream ref")
	requireNoErr(t, repo.SetActualCost(ctx, task.ID, 2), "repeat set actual cost")

	// 但任务不存在时必须报出来，否则执行侧会以为自己记下了实扣额度。
	requireCode(t, repo.SetActualCost(ctx, uid.New(), 1), domain.CodeNotFound)
	requireCode(t, repo.SetUpstreamRef(ctx, uid.New(), "ref", "raw"), domain.CodeNotFound)
}

// TestTaskRepoStats 钉住 Stats 的两条语义：label 原样回填，
// queued/running 与最老积压不受统计窗口约束。
func TestTaskRepoStats(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewTaskRepo(db)
	f := newFixture(t, 1000, 1<<30)

	queued := f.newTask(t, 1)
	runningTask := f.newTask(t, 1)
	requireNoErr(t, repo.UpdateStatus(ctx, runningTask.ID, domain.TaskStatusQueued, domain.TaskStatusRunning, nil),
		"to running")

	stats, err := repo.Stats(ctx, 24*time.Hour, "24h")
	requireNoErr(t, err, "task stats")
	if stats.Window != "24h" {
		t.Errorf("window label = %q, want the caller's label verbatim", stats.Window)
	}
	if stats.Queued < 1 {
		t.Errorf("queued = %d, want at least the one we just created", stats.Queued)
	}
	if stats.Running < 1 {
		t.Errorf("running = %d, want at least the one we just started", stats.Running)
	}
	// 积压是否恶化的唯一信号，队列非空时它必须有值。
	if stats.OldestQueuedAgeSeconds == nil {
		t.Error("oldest_queued_age_seconds must be present while the queue is non-empty")
	}
	if stats.ByStatus == nil || stats.ByErrorCode == nil {
		t.Error("ByStatus / ByErrorCode must be non-nil maps so the JSON is an object, not null")
	}
	_ = queued
}

// TestEventRepo 覆盖 SSE 事件的追加、补发与清理。
func TestEventRepo(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewEventRepo(db)
	f := newFixture(t, 0, 1<<30)

	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := repo.Append(ctx, stream.Event{
			UserID: f.userID,
			Type:   stream.EventTaskUpdated,
			Data:   map[string]any{"seq": i},
		})
		requireNoErr(t, err, "append event")
		ids = append(ids, id)
	}

	// id 必须单调递增：补发靠 Last-Event-ID 定位，乱序就会漏发或重发。
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("event ids are not monotonic: %v", ids)
		}
	}

	// 断线补发：只要 afterID 之后的。
	after, err := repo.ListAfter(ctx, f.userID, ids[0], 100)
	requireNoErr(t, err, "list after")
	if len(after) != 2 {
		t.Fatalf("ListAfter returned %d events, want 2", len(after))
	}
	if after[0].ID != ids[1] || after[1].ID != ids[2] {
		t.Errorf("ListAfter is not ordered by id: %d, %d", after[0].ID, after[1].ID)
	}
	// Data 是 any，JSON 往返后是 map[string]any。
	data, ok := after[0].Data.(map[string]any)
	if !ok {
		t.Fatalf("event data came back as %T, want map[string]any", after[0].Data)
	}
	if data["seq"] != float64(1) {
		t.Errorf("event payload lost in round trip: %+v", data)
	}
	if after[0].CreatedAt.Location() != time.UTC {
		t.Errorf("event created_at is not UTC: %v", after[0].CreatedAt)
	}

	// 从 0 开始补发拿到全部。
	all, err := repo.ListAfter(ctx, f.userID, 0, 100)
	requireNoErr(t, err, "list all")
	if len(all) != 3 {
		t.Errorf("ListAfter(0) returned %d events, want 3", len(all))
	}

	// 事件是短期缓冲不是审计日志，过期就该清掉。
	n, err := repo.DeleteBefore(ctx, time.Now().UTC().Add(time.Hour))
	requireNoErr(t, err, "delete before")
	if n < 3 {
		t.Errorf("DeleteBefore removed %d events, want at least the 3 we created", n)
	}
	left, err := repo.ListAfter(ctx, f.userID, 0, 100)
	requireNoErr(t, err, "list after cleanup")
	if len(left) != 0 {
		t.Errorf("%d events survived the cleanup", len(left))
	}
}

// TestUploadRepo 覆盖临时上传对象的提升与过期回收。
func TestUploadRepo(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewUploadRepo(db)
	f := newFixture(t, 0, 1<<30)

	expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)
	u, err := repo.Create(ctx, domain.Upload{
		UserID:     f.userID,
		StorageKey: "uploads/" + uid.Token(8) + ".png",
		MIME:       "image/png",
		Bytes:      1024,
		ExpiresAt:  expires,
	})
	requireNoErr(t, err, "create upload")
	if u.ID == "" {
		t.Fatal("upload id was not assigned")
	}
	if !u.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", u.ExpiresAt, expires)
	}
	if u.AssetID != nil {
		t.Errorf("a fresh upload must not be promoted yet: %v", *u.AssetID)
	}

	// 过期扫描：还没到期的不该被扫出来。
	early, err := repo.ListExpired(ctx, time.Now().UTC(), 100)
	requireNoErr(t, err, "list expired early")
	for _, e := range early {
		if e.ID == u.ID {
			t.Error("an upload that has not expired yet was listed for cleanup")
		}
	}

	expiredNow, err := repo.ListExpired(ctx, expires.Add(time.Second), 100)
	requireNoErr(t, err, "list expired")
	found := false
	for _, e := range expiredNow {
		if e.ID == u.ID {
			found = true
		}
	}
	if !found {
		t.Error("expired upload was not listed for cleanup")
	}

	// 提升为 asset 之后就不再是临时对象，清理任务必须放过它，
	// 否则会把一件已经被引用的资产的字节删掉。
	a := f.newAsset(t, 1024)
	requireNoErr(t, repo.MarkPromoted(ctx, u.ID, a.ID), "mark promoted")
	promoted, err := repo.Get(ctx, u.ID)
	requireNoErr(t, err, "get upload")
	if promoted.AssetID == nil || *promoted.AssetID != a.ID {
		t.Fatalf("asset_id not persisted: %v", promoted.AssetID)
	}

	stillExpired, err := repo.ListExpired(ctx, expires.Add(time.Second), 100)
	requireNoErr(t, err, "list expired after promotion")
	for _, e := range stillExpired {
		if e.ID == u.ID {
			t.Error("a promoted upload must never be listed for cleanup")
		}
	}

	requireNoErr(t, repo.Delete(ctx, u.ID), "delete upload")
	requireCode(t, func() error { _, err := repo.Get(ctx, u.ID); return err }(), domain.CodeNotFound)
	requireCode(t, repo.Delete(ctx, u.ID), domain.CodeNotFound)
	requireCode(t, repo.MarkPromoted(ctx, uid.New(), a.ID), domain.CodeNotFound)
}
