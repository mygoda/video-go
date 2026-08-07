package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// TestAssetRepoSoftDelete 钉住接口注释里最吃紧的一条：
// Delete 是软删——打标记 + 释放配额，字节留给 admin 清理任务。
func TestAssetRepoSoftDelete(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(db)
	f := newFixture(t, 0, 1<<30)

	a := f.newAsset(t, 4096)
	if a.Source == nil || a.Source.ModelID != f.modelID {
		t.Errorf("source snapshot lost: %+v", a.Source)
	}
	if a.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is not UTC: %v", a.CreatedAt)
	}

	bytesBefore, countBefore, err := repo.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum bytes")
	if bytesBefore != 4096 || countBefore != 1 {
		t.Fatalf("SumBytes = (%d, %d), want (4096, 1)", bytesBefore, countBefore)
	}

	requireNoErr(t, repo.Delete(ctx, a.ID), "soft delete asset")

	// 行还在，只是打了标记——血缘边靠它活着。
	var deletedAt *time.Time
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT deleted_at FROM assets WHERE id = ?`, a.ID).Scan(&deletedAt),
		"the row must survive a soft delete")
	if deletedAt == nil {
		t.Fatal("Delete did not stamp deleted_at; it must be a soft delete")
	}

	// 但所有读路径都要当它不存在。
	requireCode(t, func() error { _, err := repo.Get(ctx, a.ID); return err }(), domain.CodeNotFound)
	page, err := repo.List(ctx, f.userID, "", "", "", 100)
	requireNoErr(t, err, "list assets")
	for _, item := range page.Items {
		if item.ID == a.ID {
			t.Error("soft-deleted asset leaked into List")
		}
	}

	// 配额立刻释放：用户删掉东西就该马上能再传，不必等清理任务跑。
	bytesAfter, countAfter, err := repo.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum bytes after delete")
	if bytesAfter != 0 || countAfter != 0 {
		t.Errorf("SumBytes = (%d, %d) after soft delete, want (0, 0)", bytesAfter, countAfter)
	}
	var used int64
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM users WHERE id = ?`, f.userID).Scan(&used), "read storage used")
	if used != 0 {
		t.Errorf("storage_used_bytes = %d after soft delete, want 0", used)
	}

	// 重复删不该把配额释放两次——那会让用量算成负数。
	requireCode(t, repo.Delete(ctx, a.ID), domain.CodeNotFound)
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM users WHERE id = ?`, f.userID).Scan(&used), "read storage used")
	if used != 0 {
		t.Errorf("a second delete moved storage_used_bytes to %d", used)
	}

	// 软删过的才轮得到清理任务回收字节。
	soft, err := repo.ListSoftDeleted(ctx, time.Now().UTC().Add(time.Hour), 100)
	requireNoErr(t, err, "list soft deleted")
	found := false
	for _, s := range soft {
		if s.ID == a.ID {
			found = true
			if s.StorageKey == "" {
				t.Error("cleanup needs the storage key to reclaim the bytes")
			}
		}
	}
	if !found {
		t.Error("soft-deleted asset was not listed for cleanup")
	}

	requireNoErr(t, repo.HardDelete(ctx, a.ID), "hard delete")
	requireCode(t, repo.HardDelete(ctx, a.ID), domain.CodeNotFound)
	requireCode(t, repo.Delete(ctx, uid.New()), domain.CodeNotFound)
}

// TestAssetRepoHardDeleteGuard 钉住硬删只走清理路径：没软删过的资产不能被物理删掉。
func TestAssetRepoHardDeleteGuard(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(db)
	f := newFixture(t, 0, 1<<30)

	a := f.newAsset(t, 1024)
	// 一个走错路的 admin 调用不该把用户还在用的资产连同血缘一起抹掉。
	requireCode(t, repo.HardDelete(ctx, a.ID), domain.CodeConflict)

	if _, err := repo.Get(ctx, a.ID); err != nil {
		t.Fatalf("asset was hard-deleted despite never being soft-deleted: %v", err)
	}
}

// TestAssetRepoLineage 钉住血缘遍历：深度上限、去重、有环也要停。
func TestAssetRepoLineage(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(db)
	f := newFixture(t, 0, 1<<30)

	a := f.newAsset(t, 100)
	b := f.newAsset(t, 100)
	c := f.newAsset(t, 100)

	requireNoErr(t, repo.AddLineage(ctx, []domain.LineageEdge{
		{From: a.ID, To: b.ID, Relation: domain.LineageInput},
		{From: b.ID, To: c.ID, Relation: domain.LineageInput},
		// 一条回边，把图变成有环的。遍历必须靠 visited 集合停下来，
		// 否则这里就是一个死循环。
		{From: c.ID, To: a.ID, Relation: domain.LineageInput},
	}), "add lineage")

	// 重复加同一条边是常态（重试），不该炸。
	requireNoErr(t, repo.AddLineage(ctx, []domain.LineageEdge{
		{From: a.ID, To: b.ID, Relation: domain.LineageInput},
	}), "re-add the same edge")

	// 空切片是合法输入。
	requireNoErr(t, repo.AddLineage(ctx, nil), "add empty lineage")

	down, err := repo.Lineage(ctx, a.ID, domain.LineageDescendants, 5)
	requireNoErr(t, err, "lineage descendants")
	if !lineageHasNode(down, b.ID) || !lineageHasNode(down, c.ID) {
		t.Errorf("descendants missing nodes: %+v", lineageNodeIDs(down))
	}
	assertNoDuplicateNodes(t, down)

	up, err := repo.Lineage(ctx, c.ID, domain.LineageAncestors, 5)
	requireNoErr(t, err, "lineage ancestors")
	if !lineageHasNode(up, b.ID) || !lineageHasNode(up, a.ID) {
		t.Errorf("ancestors missing nodes: %+v", lineageNodeIDs(up))
	}
	assertNoDuplicateNodes(t, up)

	both, err := repo.Lineage(ctx, b.ID, domain.LineageBoth, 5)
	requireNoErr(t, err, "lineage both")
	if !lineageHasNode(both, a.ID) || !lineageHasNode(both, c.ID) {
		t.Errorf("both-direction lineage missing nodes: %+v", lineageNodeIDs(both))
	}
	assertNoDuplicateNodes(t, both)

	// 深度 1：只走一跳，拿不到隔了两跳的 c。
	shallow, err := repo.Lineage(ctx, a.ID, domain.LineageDescendants, 1)
	requireNoErr(t, err, "shallow lineage")
	if lineageHasNode(shallow, c.ID) {
		t.Error("depth=1 traversal reached a node two hops away")
	}

	// 软删一件资产之后，血缘边还在——那正是软删存在的理由。
	requireNoErr(t, repo.Delete(ctx, b.ID), "soft delete middle node")
	stillThere, err := repo.Lineage(ctx, a.ID, domain.LineageDescendants, 5)
	requireNoErr(t, err, "lineage after soft delete")
	if len(stillThere.Edges) == 0 {
		t.Error("soft delete destroyed the lineage edges it was designed to preserve")
	}
}

func lineageNodeIDs(g domain.LineageGraph) []string {
	out := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		out = append(out, n.ID)
	}
	return out
}

func lineageHasNode(g domain.LineageGraph, id string) bool {
	for _, n := range g.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func assertNoDuplicateNodes(t *testing.T, g domain.LineageGraph) {
	t.Helper()
	seen := map[string]bool{}
	for _, n := range g.Nodes {
		if seen[n.ID] {
			t.Errorf("node %s appears twice; the traversal is not deduping", n.ID)
		}
		seen[n.ID] = true
	}
}

// TestAssetRepoListAndVariants 覆盖分页、按任务过滤与派生规格落库。
func TestAssetRepoListAndVariants(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(db)
	f := newFixture(t, 100, 1<<30)

	task := f.newTask(t, 1)
	const total = 5
	for i := 0; i < total; i++ {
		a := f.newAsset(t, 10)
		if i == 0 {
			_, err := db.ExecContext(ctx, `UPDATE assets SET task_id = ? WHERE id = ?`, task.ID, a.ID)
			requireNoErr(t, err, "attach asset to task")
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		p, err := repo.List(ctx, f.userID, "", "", cursor, 2)
		requireNoErr(t, err, "list assets")
		for _, a := range p.Items {
			if seen[a.ID] {
				t.Fatalf("asset %s appeared on two pages", a.ID)
			}
			seen[a.ID] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged over %d assets, want %d", len(seen), total)
	}

	// 垃圾游标降级成第一页。
	p, err := repo.List(ctx, f.userID, "", "", "!!!garbage!!!", 100)
	requireNoErr(t, err, "list with garbage cursor")
	if len(p.Items) != total {
		t.Errorf("garbage cursor returned %d assets, want %d", len(p.Items), total)
	}

	byTask, err := repo.ListByTask(ctx, task.ID)
	requireNoErr(t, err, "list by task")
	if len(byTask) != 1 {
		t.Errorf("ListByTask returned %d assets, want 1", len(byTask))
	}

	// 类型过滤：库里这批都是 image，问 video 就该是空的。
	videos, err := repo.List(ctx, f.userID, domain.AssetTypeVideo, "", "", 100)
	requireNoErr(t, err, "list videos")
	if len(videos.Items) != 0 {
		t.Errorf("type filter matched %d assets, want 0", len(videos.Items))
	}

	// 派生规格是转存之后独立的一步，且允许失败，因此单独落库。
	target := byTask[0]
	thumb := "thumbs/" + uid.Token(6) + ".webp"
	requireNoErr(t, repo.SetVariants(ctx, target.ID, &thumb, nil), "set variants")
	withThumb, err := repo.Get(ctx, target.ID)
	requireNoErr(t, err, "re-read asset")
	if withThumb.ThumbKey == nil || *withThumb.ThumbKey != thumb {
		t.Errorf("thumb_key not persisted: %v", withThumb.ThumbKey)
	}
	if withThumb.PosterKey != nil {
		t.Errorf("poster_key must stay nil when it was not supplied: %v", *withThumb.PosterKey)
	}

	// 重复落同一份派生结果是重试的常态，不算失败。
	requireNoErr(t, repo.SetVariants(ctx, target.ID, &thumb, nil), "repeat set variants")
	// 但资产不存在时要报出来，否则 Deriver 以为缩略图挂上去了而前端永远拿不到。
	requireCode(t, repo.SetVariants(ctx, uid.New(), &thumb, nil), domain.CodeNotFound)

	usage, err := repo.StorageUsage(ctx, 5)
	requireNoErr(t, err, "storage usage")
	if usage.TotalBytes <= 0 || usage.AssetCount <= 0 {
		t.Errorf("storage usage looks empty: %+v", usage)
	}
}

// TestQuotaGuard 覆盖预占 / 落实 / 释放 / 重算这四步。
func TestQuotaGuard(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const quota = 10000
	f := newFixture(t, 0, quota)
	guard := NewQuotaGuard(db)

	usage, err := guard.Usage(ctx, f.userID)
	requireNoErr(t, err, "usage")
	if usage.QuotaBytes != quota || usage.UsedBytes != 0 {
		t.Fatalf("initial usage = %+v", usage)
	}

	requireNoErr(t, guard.Reserve(ctx, f.userID, 4000), "reserve")
	usage, err = guard.Usage(ctx, f.userID)
	requireNoErr(t, err, "usage after reserve")
	if usage.UsedBytes != 4000 {
		t.Errorf("used = %d after reserve, want 4000", usage.UsedBytes)
	}

	// 超配额必须在转存**之前**拦住：产物落盘之后再发现超限，
	// 那次上游生成的钱和带宽就白花了。
	requireCode(t, guard.Reserve(ctx, f.userID, quota), domain.CodeQuotaExceeded)
	usage, err = guard.Usage(ctx, f.userID)
	requireNoErr(t, err, "usage after rejected reserve")
	if usage.UsedBytes != 4000 {
		t.Errorf("a rejected reserve moved usage to %d", usage.UsedBytes)
	}

	// 预估与实际总有偏差，Commit 多退少补。
	requireNoErr(t, guard.Commit(ctx, f.userID, 4000, 2500), "commit smaller actual")
	usage, err = guard.Usage(ctx, f.userID)
	requireNoErr(t, err, "usage after commit")
	if usage.UsedBytes != 2500 {
		t.Errorf("used = %d after commit, want 2500", usage.UsedBytes)
	}

	requireNoErr(t, guard.Release(ctx, f.userID, 2500), "release")
	usage, err = guard.Usage(ctx, f.userID)
	requireNoErr(t, err, "usage after release")
	if usage.UsedBytes != 0 {
		t.Errorf("used = %d after release, want 0", usage.UsedBytes)
	}

	// 释放得比占用的多，用量要压到 0 而不是变成负数——
	// 负的用量会让下一次配额检查算出一个虚高的可用空间。
	requireNoErr(t, guard.Release(ctx, f.userID, 999999), "over-release")
	usage, err = guard.Usage(ctx, f.userID)
	requireNoErr(t, err, "usage after over-release")
	if usage.UsedBytes != 0 {
		t.Errorf("over-release drove usage to %d; it must clamp at 0", usage.UsedBytes)
	}

	// 累加出来的用量是缓存，逐条求和才是真相。
	a := f.newAsset(t, 777)
	recomputed, err := guard.Recompute(ctx, f.userID)
	requireNoErr(t, err, "recompute")
	if recomputed.UsedBytes != 777 || recomputed.AssetCount != 1 {
		t.Errorf("recomputed usage = %+v, want 777 bytes / 1 asset", recomputed)
	}
	var cached int64
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM users WHERE id = ?`, f.userID).Scan(&cached), "read cache")
	if cached != 777 {
		t.Errorf("recompute did not write the corrected value back: %d", cached)
	}

	// 软删过的资产不该再算进用量。
	requireNoErr(t, NewAssetRepo(db).Delete(ctx, a.ID), "soft delete")
	recomputed, err = guard.Recompute(ctx, f.userID)
	requireNoErr(t, err, "recompute after soft delete")
	if recomputed.UsedBytes != 0 {
		t.Errorf("soft-deleted asset still counts toward the quota: %+v", recomputed)
	}

	requireCode(t, guard.Reserve(ctx, uid.New(), 1), domain.CodeNotFound)
	requireCode(t, func() error { _, err := guard.Usage(ctx, uid.New()); return err }(), domain.CodeNotFound)
}
