package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// newProject 造一个空项目，测试用例各自造自己的。
func newProject(t *testing.T, f *fixture) domain.Project {
	t.Helper()
	p, err := NewCanvasRepo(f.db).CreateProject(context.Background(), domain.Project{
		UserID: f.userID,
		Name:   "test project " + uid.Token(6),
	})
	requireNoErr(t, err, "create project")
	return p
}

// TestCanvasProjectCRUD 覆盖项目的增改查删与分页。
func TestCanvasProjectCRUD(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewCanvasRepo(db)
	f := newFixture(t, 0, 1<<30)

	p := newProject(t, f)
	if p.ID == "" {
		t.Fatal("project id was not assigned")
	}
	// 新项目的 revision 是有意义的起点：客户端拿它作为第一次 ApplyOps 的 base。
	if p.Revision < 0 {
		t.Errorf("revision = %d, want a non-negative starting point", p.Revision)
	}
	if p.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is not UTC: %v", p.CreatedAt)
	}

	got, err := repo.GetProject(ctx, p.ID)
	requireNoErr(t, err, "get project")
	if got.Name != p.Name {
		t.Errorf("name = %q, want %q", got.Name, p.Name)
	}

	renamed := got
	renamed.Name = "renamed " + uid.Token(4)
	updated, err := repo.UpdateProject(ctx, renamed)
	requireNoErr(t, err, "update project")
	if updated.Name != renamed.Name {
		t.Errorf("rename did not persist: %q", updated.Name)
	}

	// 分页。
	for i := 0; i < 4; i++ {
		newProject(t, f)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		pg, err := repo.ListProjects(ctx, f.userID, cursor, 2)
		requireNoErr(t, err, "list projects")
		for _, item := range pg.Items {
			if seen[item.ID] {
				t.Fatalf("project %s appeared on two pages", item.ID)
			}
			seen[item.ID] = true
		}
		if pg.NextCursor == "" {
			break
		}
		cursor = pg.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged over %d projects, want 5", len(seen))
	}

	pg, err := repo.ListProjects(ctx, f.userID, "!!!garbage!!!", 100)
	requireNoErr(t, err, "list with garbage cursor")
	if len(pg.Items) != 5 {
		t.Errorf("garbage cursor returned %d projects, want 5", len(pg.Items))
	}

	// 删项目是软删：画布上的卡片指向的资产还在，历史消息也还要能显示。
	requireNoErr(t, repo.DeleteProject(ctx, p.ID), "delete project")
	requireCode(t, func() error { _, err := repo.GetProject(ctx, p.ID); return err }(), domain.CodeNotFound)
	after, err := repo.ListProjects(ctx, f.userID, "", 100)
	requireNoErr(t, err, "list after delete")
	for _, item := range after.Items {
		if item.ID == p.ID {
			t.Error("deleted project leaked into ListProjects")
		}
	}
	requireCode(t, repo.DeleteProject(ctx, p.ID), domain.CodeNotFound)
	requireCode(t, func() error { _, err := repo.GetProject(ctx, uid.New()); return err }(), domain.CodeNotFound)
}

// TestCanvasApplyOps 钉住乐观锁：baseRevision 对不上就是 revision_conflict，
// 且冲突时**一条 op 都不能落库**。
func TestCanvasApplyOps(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewCanvasRepo(db)
	f := newFixture(t, 0, 1<<30)
	p := newProject(t, f)

	cardID := "card_" + uid.Token(8)
	rev, err := repo.ApplyOps(ctx, p.ID, p.Revision, []domain.CanvasOp{
		{Type: domain.OpCardCreate, Card: &domain.Card{
			ID: cardID, Kind: domain.CardKindText, Title: "便签", X: 10, Y: 20, W: 100, H: 50, Z: 1,
			Text: strPtrOf("hello"), Refs: []string{},
		}},
	})
	requireNoErr(t, err, "apply create op")
	if rev <= p.Revision {
		t.Fatalf("revision did not advance: %d -> %d", p.Revision, rev)
	}

	snap, err := repo.Snapshot(ctx, p.ID)
	requireNoErr(t, err, "snapshot")
	if snap.Revision != rev {
		t.Errorf("snapshot revision = %d, want %d", snap.Revision, rev)
	}
	if len(snap.Cards) != 1 || snap.Cards[0].ID != cardID {
		t.Fatalf("card was not created: %+v", snap.Cards)
	}
	if snap.Cards[0].Text == nil || *snap.Cards[0].Text != "hello" {
		t.Errorf("card text lost: %v", snap.Cards[0].Text)
	}
	// title 是后加的列，钉住它是因为它此前整条链路都不存在：
	// 前端一直在发，后端一路静默丢弃，用户改完标题一刷新就没了。
	if snap.Cards[0].Title != "便签" {
		t.Errorf("card title lost: %q", snap.Cards[0].Title)
	}
	// Refs 在 API 里是 required，NULL 也要投影成空数组而不是 null。
	if snap.Cards[0].Refs == nil {
		t.Error("card refs must never come back nil")
	}

	// 两个客户端拿着同一个 base 提交，第二个必须撞上 revision_conflict——
	// 那正是它该重新拉全量再重放本地改动的信号。
	staleBase := p.Revision
	requireCode(t, func() error {
		_, err := repo.ApplyOps(ctx, p.ID, staleBase, []domain.CanvasOp{
			{Type: domain.OpCardMove, ID: cardID, X: f64(1), Y: f64(2)},
		})
		return err
	}(), domain.CodeRevisionConflict)

	// 冲突的那批 op 一条都不该落库。
	afterConflict, err := repo.Snapshot(ctx, p.ID)
	requireNoErr(t, err, "snapshot after conflict")
	if afterConflict.Revision != rev {
		t.Errorf("a rejected batch moved the revision to %d", afterConflict.Revision)
	}
	if afterConflict.Cards[0].X != 10 || afterConflict.Cards[0].Y != 20 {
		t.Errorf("a rejected batch was partially applied: x=%v y=%v",
			afterConflict.Cards[0].X, afterConflict.Cards[0].Y)
	}

	// move / update / viewport 一批按序应用。
	rev2, err := repo.ApplyOps(ctx, p.ID, rev, []domain.CanvasOp{
		{Type: domain.OpCardMove, ID: cardID, X: f64(111), Y: f64(222), Z: f64(3)},
		{Type: domain.OpCardUpdate, ID: cardID, Patch: map[string]any{"w": 300.0, "prompt": "a cat", "title": "改过的标题"}},
		{Type: domain.OpViewportSet, Viewport: &domain.Viewport{X: 5, Y: 6, K: 1.5}},
	})
	requireNoErr(t, err, "apply a batch")
	if rev2 <= rev {
		t.Fatalf("revision did not advance: %d -> %d", rev, rev2)
	}

	snap, err = repo.Snapshot(ctx, p.ID)
	requireNoErr(t, err, "snapshot")
	card := snap.Cards[0]
	if card.X != 111 || card.Y != 222 || card.Z != 3 {
		t.Errorf("move was not applied: %+v", card)
	}
	if card.W != 300 {
		t.Errorf("patch w = %v, want 300", card.W)
	}
	if card.Prompt == nil || *card.Prompt != "a cat" {
		t.Errorf("patch prompt = %v", card.Prompt)
	}
	if card.Title != "改过的标题" {
		t.Errorf("patch title = %q", card.Title)
	}
	if snap.Viewport == nil || snap.Viewport.K != 1.5 {
		t.Errorf("viewport was not persisted: %+v", snap.Viewport)
	}

	// 未知 key 直接报错而不是静默忽略：忽略会让客户端以为改成功了。
	requireCode(t, func() error {
		_, err := repo.ApplyOps(ctx, p.ID, rev2, []domain.CanvasOp{
			{Type: domain.OpCardUpdate, ID: cardID, Patch: map[string]any{"project_id": "hijacked"}},
		})
		return err
	}(), domain.CodeInvalidParam)

	// 空批次不该推进版本号——否则每次心跳都会让别的客户端以为画布变了。
	same, err := repo.ApplyOps(ctx, p.ID, rev2, nil)
	requireNoErr(t, err, "apply empty batch")
	if same != rev2 {
		t.Errorf("an empty batch moved the revision from %d to %d", rev2, same)
	}

	// 删卡片。
	rev3, err := repo.ApplyOps(ctx, p.ID, rev2, []domain.CanvasOp{
		{Type: domain.OpCardDelete, ID: cardID},
	})
	requireNoErr(t, err, "apply delete op")
	snap, err = repo.Snapshot(ctx, p.ID)
	requireNoErr(t, err, "snapshot after delete")
	if len(snap.Cards) != 0 {
		t.Errorf("card survived the delete op: %+v", snap.Cards)
	}
	if snap.Revision != rev3 {
		t.Errorf("snapshot revision = %d, want %d", snap.Revision, rev3)
	}

	// 落库的这串 op 是「创作过程可回放」的数据基础，必须真的在库里。
	var opBatches int
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM canvas_ops WHERE project_id = ?`, p.ID).Scan(&opBatches),
		"count persisted ops")
	if opBatches == 0 {
		t.Error("no ops were persisted; the replay history is empty")
	}

	requireCode(t, func() error {
		_, err := repo.ApplyOps(ctx, uid.New(), 0, []domain.CanvasOp{{Type: domain.OpCardDelete, ID: "x"}})
		return err
	}(), domain.CodeNotFound)
}

// TestCanvasAppendMessage 覆盖对话消息的追加与快照回读。
func TestCanvasAppendMessage(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewCanvasRepo(db)
	f := newFixture(t, 0, 1<<30)
	p := newProject(t, f)

	m, err := repo.AppendMessage(ctx, domain.Message{
		ProjectID:  p.ID,
		Role:       domain.MessageRoleUser,
		Content:    "make me a cat",
		RefCardIDs: []string{"card_a", "card_b"},
	})
	requireNoErr(t, err, "append message")
	if m.ID == "" {
		t.Fatal("message id was not assigned")
	}
	if m.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is not UTC: %v", m.CreatedAt)
	}

	_, err = repo.AppendMessage(ctx, domain.Message{
		ProjectID: p.ID,
		Role:      domain.MessageRoleAssistant,
		Content:   "here you go",
	})
	requireNoErr(t, err, "append assistant message")

	snap, err := repo.Snapshot(ctx, p.ID)
	requireNoErr(t, err, "snapshot")
	if len(snap.Conversation) != 2 {
		t.Fatalf("conversation has %d messages, want 2", len(snap.Conversation))
	}
	// 对话必须按时间正序，否则前端渲染出来是倒的。
	if snap.Conversation[0].ID != m.ID {
		t.Errorf("conversation is not in chronological order")
	}
	if got := snap.Conversation[0].RefCardIDs; len(got) != 2 || got[0] != "card_a" {
		t.Errorf("ref_card_ids lost in round trip: %+v", got)
	}

	requireCode(t, func() error {
		_, err := repo.AppendMessage(ctx, domain.Message{ProjectID: uid.New(), Role: domain.MessageRoleUser, Content: "x"})
		return err
	}(), domain.CodeNotFound)

	// 软删过的项目同样是"不在"：外键只看行在不在，看不到 deleted_at，
	// 不显式挡住的话消息会被追加到一个谁也打不开的画布上。
	requireNoErr(t, repo.DeleteProject(ctx, p.ID), "delete project")
	requireCode(t, func() error {
		_, err := repo.AppendMessage(ctx, domain.Message{ProjectID: p.ID, Role: domain.MessageRoleUser, Content: "x"})
		return err
	}(), domain.CodeNotFound)
}

func f64(v float64) *float64 { return &v }

func strPtrOf(s string) *string { return &s }
