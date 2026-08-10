package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// 本文件守的是一条跨层不变量：**一次转存对 users.storage_used_bytes 的净效果
// 恰好等于一次实际字节数**。
//
// 它只能在这一层测：预占走 QuotaGuard、真实计费走 AssetRepo.Create，两者是
// 不同的对象、不同的事务，各自的单元测试都能通过而合起来仍然记两遍——
// 那正是 DEM-98 的病灶。因此断言的对象不是某个方法的返回值，而是
// 「缓存列」与「assets 逐条求和」这两个数**必须相等**。

// transferHarness 把真实仓储、真实配额守卫和一个临时文件存储装成一台转存器。
type transferHarness struct {
	tr       asset.Transferor
	assets   store.AssetRepo
	uploads  store.UploadRepo
	blobs    asset.Store
	blobRoot string
}

func newTransferHarness(t *testing.T, db *sql.DB, assets store.AssetRepo) *transferHarness {
	t.Helper()
	root := t.TempDir()
	blobs, err := asset.NewFSStore(root)
	requireNoErr(t, err, "new fs store")

	uploads := NewUploadRepo(db)
	tr, err := asset.NewTransferor(asset.TransferorDeps{
		Blobs:   blobs,
		Assets:  assets,
		Uploads: uploads,
		Quota:   NewQuotaGuard(db),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	requireNoErr(t, err, "new transferor")
	return &transferHarness{tr: tr, assets: assets, uploads: uploads, blobs: blobs, blobRoot: root}
}

// storedObjects 数一数存储里落了几个对象，用来证明「配额被拒时字节没落盘」。
func (h *transferHarness) storedObjects(t *testing.T) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(h.blobRoot, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	requireNoErr(t, err, "walk blob root")
	return n
}

// b64Artifact 造一件内联产物。
//
// 走 KindBase64 而不是 KindURL：不需要驱动、不发网络请求，字节数完全由用例
// 说了算，而这些用例断言的正是"那个数最后有没有被记两遍"。
//
// declared 模拟上游自报的大小（Transfer 拿它作预占估值）。给 0 表示上游没报，
// 预占会退到实际大小——预占与实际相等时 Commit 的差额是 0，重复累加反而更
// 难被"多退少补"的算术掩盖，因此两种情况都要覆盖。
func b64Artifact(index, size int, declared int64) adapter.ArtifactRef {
	raw := bytes.Repeat([]byte{'x'}, size)
	return adapter.ArtifactRef{
		Kind:   adapter.KindBase64,
		Type:   domain.AssetTypeImage,
		MIME:   "image/png",
		Base64: base64.StdEncoding.EncodeToString(raw),
		Bytes:  declared,
		Index:  index,
	}
}

// cachedUsage 直接读缓存列，绕开 Usage()，避免用被测代码解释被测数据。
func cachedUsage(t *testing.T, db *sql.DB, userID string) int64 {
	t.Helper()
	var used int64
	requireNoErr(t, db.QueryRowContext(context.Background(),
		`SELECT storage_used_bytes FROM users WHERE id = ?`, userID).Scan(&used), "read cached usage")
	return used
}

// TestTransferChargesStorageExactlyOnce 是 DEM-98 的回归用例。
//
// 800235 是 issue 里那次真实 seedream 出图的字节数，当时缓存列停在
// 1600470（正好 2 倍）。这里断言的是两个数**精确相等**而不是"差不多"：
// 重复累加是确定性的，容差只会把它放过去。
func TestTransferChargesStorageExactlyOnce(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	const actual = 800235
	a, err := h.tr.Transfer(ctx, f.userID, "", b64Artifact(0, actual, 0), adapter.PollRequest{})
	requireNoErr(t, err, "transfer")
	if a.Bytes != actual {
		t.Fatalf("落库的资产大小 = %d，want %d", a.Bytes, actual)
	}

	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("users.storage_used_bytes = %d，SUM(assets.bytes) = %d（%d 件）", cached, sum, count)

	if count != 1 {
		t.Fatalf("应只落 1 件资产，实际 %d 件", count)
	}
	if sum != actual {
		t.Fatalf("SUM(assets.bytes) = %d，want %d", sum, actual)
	}
	if cached != sum {
		t.Fatalf("一次转存把用量记了 %.3f 遍：storage_used_bytes = %d，SUM(assets.bytes) = %d",
			float64(cached)/float64(sum), cached, sum)
	}
}

// TestTransferUsageStaysExactAcrossManyArtifacts 验证累加不漂。
//
// 单件对得上还不够：Commit 的差额是带符号的，只测一件时"预占估大了"与
// "多加了一遍"可能互相抵消看起来正常。多件、且估值有大有小才逼得出真相。
// 其中一半并发跑：预占走 SELECT ... FOR UPDATE，真实计费走另一个事务里的
// 增量更新，交错执行时任何一处丢更新都会让两个数分家。
func TestTransferUsageStaysExactAcrossManyArtifacts(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	artifacts := []struct {
		size     int
		declared int64 // 上游自报大小，0 = 没报
	}{
		{1, 0},           // 最小非零
		{4096, 8192},     // 上游报大了
		{800235, 0},      // issue 里那次真实转存
		{32771, 16},      // 上游报小了
		{5, 5},           // 报得刚好
		{123456, 500000}, // 报大很多
	}

	// 期望值由用例自己算，不回读数据库——抄回跑出来的数就等于没断言。
	var want int64
	for _, art := range artifacts {
		want += int64(art.size)
	}
	const wantLiteral = 960564 // 1 + 4096 + 800235 + 32771 + 5 + 123456
	if want != wantLiteral {
		t.Fatalf("用例自身算错了：累计 %d，手算 %d", want, wantLiteral)
	}

	// 前一半顺序跑，后一半并发跑。
	const serial = 3
	for i := 0; i < serial; i++ {
		_, err := h.tr.Transfer(ctx, f.userID, "",
			b64Artifact(i, artifacts[i].size, artifacts[i].declared), adapter.PollRequest{})
		requireNoErr(t, err, "serial transfer")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := serial; i < len(artifacts); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := transferRetryingDeadlock(t, h, f.userID,
				b64Artifact(i, artifacts[i].size, artifacts[i].declared)); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		t.Fatalf("concurrent transfer: %v", err)
	}

	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("%d 件产物后：users.storage_used_bytes = %d，SUM(assets.bytes) = %d，手算 = %d",
		count, cached, sum, want)

	if count != len(artifacts) {
		t.Fatalf("应落 %d 件资产，实际 %d 件", len(artifacts), count)
	}
	if sum != want {
		t.Fatalf("SUM(assets.bytes) = %d，want %d", sum, want)
	}
	if cached != want {
		t.Fatalf("缓存用量 = %d，want %d（逐条求和 %d，比值 %.3f）",
			cached, want, sum, float64(cached)/float64(sum))
	}
}

// erLockDeadlock 声明在同包的 asset_concurrency_test.go 里。

// transferRetryingDeadlock 转存一件产物，只在撞上 InnoDB 死锁时重试。
//
// 并发落库会撞死锁：INSERT assets 为了校验外键先拿 users 行的 S 锁，随后同事务
// 的 UPDATE users 要升级成 X 锁，两个并发事务各持一把 S 等对方放手就成环。
// 那是 DEM-96（把 UPDATE users 挪到 INSERT 之前）负责的事，与本用例要守的
// 字节记账无关，因此这里只把它当可重试错误吞掉并记一笔。
//
// 重试不会削弱断言：死锁的那一次整个事务已回滚，预占也在失败路径上还了回去，
// 重来一次仍然只该产生一份字节。用量记两遍的话，重试多少次都对不上。
func transferRetryingDeadlock(t *testing.T, h *transferHarness, userID string, ref adapter.ArtifactRef) error {
	t.Helper()
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		if _, err = h.tr.Transfer(context.Background(), userID, "", ref, adapter.PollRequest{}); err == nil {
			return nil
		}
		var me *mysqldriver.MySQLError
		if !errors.As(err, &me) || me.Number != erLockDeadlock {
			return err
		}
		t.Logf("第 %d 次转存撞上 InnoDB 死锁（DEM-96 的范畴），重试", i+1)
	}
	return err
}

// TestTransferQuotaGateRejectsBeforeAnyByteLands 守住配额闸的**时机**。
//
// 修用量重复累加时最容易顺手把检查挪到落盘之后（那样算术更简单），但产物
// 一旦下载落盘，那次上游生成的钱和带宽就已经花掉了，再报超配额只能删掉重来。
// 因此这里不止断言"报了 quota_exceeded"，还要断言存储里一个对象都没有。
func TestTransferQuotaGateRejectsBeforeAnyByteLands(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const quota = 10000
	f := newFixture(t, 0, quota)
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	_, err := h.tr.Transfer(ctx, f.userID, "", b64Artifact(0, 2*quota, 0), adapter.PollRequest{})
	requireCode(t, err, domain.CodeQuotaExceeded)

	if n := h.storedObjects(t); n != 0 {
		t.Fatalf("超配额被拒后存储里落了 %d 个对象；配额检查必须在转存之前", n)
	}
	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	if count != 0 || sum != 0 {
		t.Fatalf("超配额被拒却落了库：%d 件 / %d 字节", count, sum)
	}
	if cached := cachedUsage(t, db, f.userID); cached != 0 {
		t.Fatalf("被拒的转存把用量推到了 %d，应保持 0", cached)
	}
}

// TestTransferReleasesReservationWhenPersistFails 验证失败路径不漏预占。
//
// 预占不还回去的话，反复失败的任务会把配额一点点吃光，而用户的资产库里
// 一件东西都没多——那是一种查不出原因的"空间莫名其妙满了"。
func TestTransferReleasesReservationWhenPersistFails(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	f := newFixture(t, 0, 1<<30)
	assets := NewAssetRepo(db)

	// 先垫一件正常资产，这样"回到原值"是个非零的数，比回到 0 更能说明问题。
	const existing = 4096
	f.newAsset(t, existing)
	before := cachedUsage(t, db, f.userID)
	if before != existing {
		t.Fatalf("垫底资产后用量 = %d，want %d", before, existing)
	}

	h := newTransferHarness(t, db, failingCreateAssets{AssetRepo: assets})
	_, err := h.tr.Transfer(ctx, f.userID, "", b64Artifact(0, 777_000, 0), adapter.PollRequest{})
	if err == nil {
		t.Fatal("落库失败时 Transfer 应报错")
	}

	sum, _, sumErr := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, sumErr, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("落库失败后：users.storage_used_bytes = %d，SUM(assets.bytes) = %d", cached, sum)

	if cached != before {
		t.Fatalf("转存失败后用量 = %d，应还回预占回到 %d", cached, before)
	}
	if cached != sum {
		t.Fatalf("失败路径让缓存与真相分家：缓存 %d，逐条求和 %d", cached, sum)
	}
}

// failingCreateAssets 的 Create 永远失败，其余方法走真实仓储。
type failingCreateAssets struct {
	store.AssetRepo
}

func (failingCreateAssets) Create(context.Context, domain.Asset) (domain.Asset, error) {
	return domain.Asset{}, errors.New("落库失败")
}

// TestPromoteChargesStorageExactlyOnce 覆盖第二条写 assets 的路径。
//
// Promote 的预占（DEM-102 的配额闸）在落库后由 Commit 原样撤掉，实际字节只由
// AssetRepo.Create 记一次，因此净效果仍是 1×——本用例守的就是加闸没有把这条
// 账改成 2×。配额给得足够大，这里断言的是记账而不是闸。
func TestPromoteChargesStorageExactlyOnce(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const size = 65536
	f := newFixture(t, 0, 1<<30)
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	up := h.newUpload(t, f.userID, size)

	a, err := h.tr.Promote(ctx, up.ID)
	requireNoErr(t, err, "promote")
	if a.Bytes != size {
		t.Fatalf("提升出来的资产大小 = %d，want %d", a.Bytes, size)
	}

	// 幂等：同一个 upload 被多个输入槽引用时会提升多次，不能因此重复计费。
	again, err := h.tr.Promote(ctx, up.ID)
	requireNoErr(t, err, "promote again")
	if again.ID != a.ID {
		t.Fatalf("重复提升产出了第二件资产：%s vs %s", again.ID, a.ID)
	}

	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("提升后：users.storage_used_bytes = %d，SUM(assets.bytes) = %d（%d 件）", cached, sum, count)

	if count != 1 || sum != size {
		t.Fatalf("提升应只落 1 件 %d 字节的资产，实际 %d 件 / %d 字节", size, count, sum)
	}
	if cached != sum {
		t.Fatalf("提升把用量记了 %.3f 遍：缓存 %d，逐条求和 %d",
			float64(cached)/float64(sum), cached, sum)
	}
}

// ── DEM-102：上传路径的配额闸 ──────────────────────────────────────────

// newUpload 造一个已落对象的临时上传，返回它的 upload 行。
func (h *transferHarness) newUpload(t *testing.T, userID string, size int) domain.Upload {
	t.Helper()
	ctx := context.Background()
	key := "uploads/" + uid.Token(8) + ".png"
	info, err := h.blobs.Put(ctx, key, bytes.NewReader(bytes.Repeat([]byte{'u'}, size)), "image/png")
	requireNoErr(t, err, "put upload object")

	up, err := h.uploads.Create(ctx, domain.Upload{
		UserID:     userID,
		StorageKey: key,
		MIME:       "image/png",
		Bytes:      info.Bytes,
	})
	requireNoErr(t, err, "create upload")
	return up
}

// TestPromoteQuotaGateRejectsBeforeAnyByteLands 是 DEM-102 的主用例：
// 配额已满的用户不能再靠提升素材写进字节，而且被拒时不留垃圾。
//
// 断言不止"报了 quota_exceeded"：一个"先复制再检查"的实现同样能返回这个错误，
// 却已经把副本写进了存储。因此这里数存储里的对象——提升前有 1 个（upload 自己），
// 被拒后必须还是 1 个，多出来的那个就是白占的磁盘。
//
// 用一件垫底资产把配额吃到只剩 2000 字节，而不是从 0 开始配一个极小的配额：
// 前者是线上真实的"用满了"，且能顺带证明被拒之后预占没有漏在缓存里。
func TestPromoteQuotaGateRejectsBeforeAnyByteLands(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const (
		quota    = 10000
		existing = 8000
		upSize   = 4000
	)
	f := newFixture(t, 0, quota)
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	f.newAsset(t, existing)
	up := h.newUpload(t, f.userID, upSize)

	before := h.storedObjects(t)
	if before != 1 {
		t.Fatalf("提升前存储里应只有 upload 自己的对象，实际 %d 个", before)
	}

	_, err := h.tr.Promote(ctx, up.ID)
	requireCode(t, err, domain.CodeQuotaExceeded)

	if n := h.storedObjects(t); n != before {
		t.Fatalf("超配额被拒后存储里多出了 %d 个对象；配额检查必须在复制之前", n-before)
	}
	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	if count != 1 || sum != existing {
		t.Fatalf("超配额被拒却落了库：%d 件 / %d 字节，want 1 件 / %d 字节", count, sum, existing)
	}
	if cached := cachedUsage(t, db, f.userID); cached != existing {
		t.Fatalf("被拒的提升让用量变成 %d，应保持 %d（预占没还回去）", cached, existing)
	}

	// upload 必须仍是未提升状态，否则 24h 后的回收扫描会跳过它，
	// 一个谁都用不上的对象就永远赖在磁盘上了。
	after, err := h.uploads.Get(ctx, up.ID)
	requireNoErr(t, err, "reload upload")
	if after.AssetID != nil {
		t.Fatalf("被拒的提升把 upload 标成了已提升：asset_id = %q", *after.AssetID)
	}
}

// TestPromoteIdempotentAtExactQuota 守住"闸不能吃掉幂等"。
//
// 配额刻意设成与素材**恰好等大**：第一次提升把它用满，第二次提升走的是
// "已提升就返回同一件"的分支。闸要是放在那个分支之前，第二次会再预占一份
// 已经计过费的字节，used + size > quota 直接误拒——而现实里这就是一个任务的
// 两个输入槽引用同一张图。
//
// 这条与闸本身无关，去掉闸它也该是绿的。
func TestPromoteIdempotentAtExactQuota(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const size = 65536
	f := newFixture(t, 0, size) // 配额与素材恰好等大
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	up := h.newUpload(t, f.userID, size)

	first, err := h.tr.Promote(ctx, up.ID)
	requireNoErr(t, err, "first promote")
	if got := cachedUsage(t, db, f.userID); got != size {
		t.Fatalf("首次提升后用量 = %d，want %d", got, size)
	}

	second, err := h.tr.Promote(ctx, up.ID)
	requireNoErr(t, err, "second promote at exact quota")
	if second.ID != first.ID {
		t.Fatalf("重复提升产出了第二件资产：%s vs %s", second.ID, first.ID)
	}

	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("两次提升后：users.storage_used_bytes = %d，SUM(assets.bytes) = %d（%d 件）", cached, sum, count)

	if count != 1 || sum != size {
		t.Fatalf("重复提升应只有 1 件 %d 字节的资产，实际 %d 件 / %d 字节", size, count, sum)
	}
	if cached != size {
		t.Fatalf("第二次提升把用量推到了 %d，want %d（幂等调用不该再占配额）", cached, size)
	}
}

// TestPromoteWithinQuotaStillSucceeds 证明加闸没有把正常路径改坏：
// 配额还剩得下的用户照旧提升成功，且用量恰好涨一件素材的大小。
//
// 剩余空间刻意只比素材大一点（剩 5000 收一个 4000），贴着边界过——
// 一个把预占算大了（比如照 Transfer 那样退到 32MB 估值）的实现会在这里被挡住。
func TestPromoteWithinQuotaStillSucceeds(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const (
		quota    = 10000
		existing = 5000
		upSize   = 4000
	)
	f := newFixture(t, 0, quota)
	assets := NewAssetRepo(db)
	h := newTransferHarness(t, db, assets)

	f.newAsset(t, existing)
	up := h.newUpload(t, f.userID, upSize)

	a, err := h.tr.Promote(ctx, up.ID)
	requireNoErr(t, err, "promote within quota")
	if a.Bytes != upSize {
		t.Fatalf("提升出来的资产大小 = %d，want %d", a.Bytes, upSize)
	}

	sum, count, err := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("配额内提升后：users.storage_used_bytes = %d，SUM(assets.bytes) = %d（%d 件）", cached, sum, count)

	if count != 2 || sum != existing+upSize {
		t.Fatalf("配额内提升应落到 2 件 / %d 字节，实际 %d 件 / %d 字节", existing+upSize, count, sum)
	}
	if cached != sum {
		t.Fatalf("配额内提升的用量记了 %.3f 遍：缓存 %d，逐条求和 %d",
			float64(cached)/float64(sum), cached, sum)
	}

	promoted, err := h.uploads.Get(ctx, up.ID)
	requireNoErr(t, err, "reload upload")
	if promoted.AssetID == nil || *promoted.AssetID != a.ID {
		t.Fatalf("提升成功后 upload 未指向新资产：%v", promoted.AssetID)
	}
}

// TestPromoteReleasesReservationWhenPersistFails 验证提升的失败路径不漏预占。
//
// 与转存同理：预占不还回去的话，反复失败的提升会把配额一点点吃光，而用户的
// 资产库里一件东西都没多。加闸引入了这条新的失败路径，因此要单独钉住。
func TestPromoteReleasesReservationWhenPersistFails(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	const (
		existing = 4096
		upSize   = 65536
	)
	f := newFixture(t, 0, 1<<30)
	assets := NewAssetRepo(db)

	f.newAsset(t, existing)
	before := cachedUsage(t, db, f.userID)
	if before != existing {
		t.Fatalf("垫底资产后用量 = %d，want %d", before, existing)
	}

	h := newTransferHarness(t, db, failingCreateAssets{AssetRepo: assets})
	up := h.newUpload(t, f.userID, upSize)

	if _, err := h.tr.Promote(ctx, up.ID); err == nil {
		t.Fatal("落库失败时 Promote 应报错")
	}

	sum, _, sumErr := assets.SumBytes(ctx, f.userID)
	requireNoErr(t, sumErr, "sum asset bytes")
	cached := cachedUsage(t, db, f.userID)
	t.Logf("提升落库失败后：users.storage_used_bytes = %d，SUM(assets.bytes) = %d", cached, sum)

	if cached != before {
		t.Fatalf("提升失败后用量 = %d，应还回预占回到 %d", cached, before)
	}
	if cached != sum {
		t.Fatalf("失败路径让缓存与真相分家：缓存 %d，逐条求和 %d", cached, sum)
	}
}
