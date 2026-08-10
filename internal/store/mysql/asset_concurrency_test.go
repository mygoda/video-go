package mysql

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// erLockDeadlock 是 InnoDB 检出死锁后回滚其中一个事务时回的错误号。
const erLockDeadlock = 1213

// isDeadlock 认 MySQL 的 1213。
//
// 不比字符串：驱动会把服务端消息原样带回来，而那句话随版本和语言变，
// 错误号不变。
func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == erLockDeadlock
}

// TestAssetRepoCreateConcurrentSameUser 钉住这条：同一个用户的多件产物并发落库
// 不许死锁，且用量缓存要精确等于字节数之和。
//
// 这不是假想的并发——批量出首帧一次排 3 镜、出片一次排 N 段，同一个 user 的
// 任务几乎同时完成、同时转存，一起走进 AssetRepo.Create。
//
// 曾经的死法：事务里先 INSERT assets（外键检查对 users 那一行加 S 锁）
// 再 UPDATE users（要 X 锁），单事务内的 S→X 升级。两个事务同时拿到 S、
// 又同时等对方放 S，InnoDB 检出死锁回滚其中一个，用户看到一次莫名其妙的失败。
func TestAssetRepoCreateConcurrentSameUser(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(db)
	f := newFixture(t, 0, 1<<40)

	const (
		rounds      = 25
		concurrency = 6
	)

	var (
		mu        sync.Mutex
		deadlocks int
		firstErr  error
		wantBytes int64
	)

	for round := range rounds {
		// 起跑线：不让 goroutine 各自慢慢开始，否则两个事务根本不会重叠，
		// 死锁窗口也就测不出来。
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range concurrency {
			bytes := int64(1000 + round*concurrency + i)
			mu.Lock()
			wantBytes += bytes
			mu.Unlock()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := repo.Create(ctx, domain.Asset{
					UserID:     f.userID,
					Type:       domain.AssetTypeImage,
					StorageKey: "test/" + uid.Token(12) + ".png",
					MIME:       "image/png",
					Bytes:      bytes,
					Source:     &domain.AssetSource{ModelID: f.modelID, Prompt: "p"},
				})
				if err == nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if isDeadlock(err) {
					deadlocks++
				}
				if firstErr == nil {
					// 连着驱动的原始错误一起留下：domain.Error 的 Error()
					// 只印自己那句话，光看它分不出死锁和别的库错。
					firstErr = err
					var me *mysql.MySQLError
					if errors.As(err, &me) {
						firstErr = errors.Join(err, me)
					}
				}
			}()
		}
		close(start)
		wg.Wait()
	}

	if deadlocks > 0 {
		t.Errorf("%d/%d 次并发落库撞上 InnoDB 死锁；第一条：%v",
			deadlocks, rounds*concurrency, firstErr)
	}
	if firstErr != nil && deadlocks == 0 {
		t.Fatalf("并发落库失败（非死锁）：%v", firstErr)
	}

	// 精确相等，不是"差不多"：用量缓存要是被并发写丢了几笔，配额就形同虚设。
	var used int64
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM users WHERE id = ?`, f.userID).Scan(&used),
		"read storage used")
	if used != wantBytes {
		t.Errorf("storage_used_bytes = %d，期望 %d（%d 件产物字节数之和）",
			used, wantBytes, rounds*concurrency)
	}

	// 缓存与真相（逐条求和）也要对得上。
	sum, count, err := repo.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum bytes")
	if sum != wantBytes || count != rounds*concurrency {
		t.Errorf("SumBytes = (%d, %d)，期望 (%d, %d)", sum, count, wantBytes, rounds*concurrency)
	}
	t.Logf("%d 轮 × %d 并发 = %d 件产物：死锁 %d 次，storage_used_bytes=%d（期望 %d），逐条求和 (%d, %d)",
		rounds, concurrency, rounds*concurrency, deadlocks, used, wantBytes, sum, count)
}

// TestAssetRepoCreateRollsBackQuotaOnFailure 钉住失败路径不漏配额。
//
// 修死锁把 UPDATE users 挪到了 INSERT assets 前面，于是出现了一个新的
// 中途失败窗口：用量已经加上去了，资产行却没落成。这条用例就守着那个窗口——
// 主键撞车让 INSERT 失败，事务回滚必须把那笔用量一起带走，
// 否则每一次失败的转存都会在配额上留下一块永远收不回来的空洞。
func TestAssetRepoCreateRollsBackQuotaOnFailure(t *testing.T) {
	db := requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(db)
	f := newFixture(t, 0, 1<<30)

	existing := f.newAsset(t, 4096)

	var before int64
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM users WHERE id = ?`, f.userID).Scan(&before),
		"read storage used")
	if before != 4096 {
		t.Fatalf("storage_used_bytes = %d，期望 4096", before)
	}

	// 复用已存在的 id：UPDATE users 会成功，紧随其后的 INSERT 撞主键失败。
	_, createErr := repo.Create(ctx, domain.Asset{
		ID:         existing.ID,
		UserID:     f.userID,
		Type:       domain.AssetTypeImage,
		StorageKey: "test/" + uid.Token(12) + ".png",
		MIME:       "image/png",
		Bytes:      777,
		Source:     &domain.AssetSource{ModelID: f.modelID, Prompt: "p"},
	})
	if createErr == nil {
		t.Fatal("重复主键的落库居然成功了")
	}
	if codeOf(createErr) != domain.CodeConflict {
		t.Errorf("重复主键应归为 conflict，实际 %q（err=%v）", codeOf(createErr), createErr)
	}

	var after int64
	requireNoErr(t, db.QueryRowContext(ctx,
		`SELECT storage_used_bytes FROM users WHERE id = ?`, f.userID).Scan(&after),
		"read storage used")
	if after != before {
		t.Errorf("失败的落库把用量从 %d 改成了 %d：事务没回滚干净", before, after)
	}

	sum, count, err := repo.SumBytes(ctx, f.userID)
	requireNoErr(t, err, "sum bytes")
	if sum != after || count != 1 {
		t.Errorf("用量缓存 %d 与逐条求和 (%d, %d) 对不上", after, sum, count)
	}
	t.Logf("落库中途失败（%v）后：storage_used_bytes=%d（失败前 %d），assets 行数 %d，逐条求和 %d",
		createErr, after, before, count, sum)
}

// TestAssetRepoCreateRejectsUnknownUser 钉住 UPDATE 前移没有把外键那道拦截弄丢。
//
// 用量更新挪到 INSERT 前面之后，不存在的 user 会先经过一条 0 行受影响的
// UPDATE——真正拦下它的仍然是 assets 的外键。这条用例守着「拦得住」，
// 免得日后有人给那条 UPDATE 加上"0 行也算成功"的语义。
func TestAssetRepoCreateRejectsUnknownUser(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := NewAssetRepo(requireDB(t))

	_, err := repo.Create(ctx, domain.Asset{
		UserID:     uid.New(),
		Type:       domain.AssetTypeImage,
		StorageKey: "test/" + uid.Token(12) + ".png",
		MIME:       "image/png",
		Bytes:      123,
		Source:     &domain.AssetSource{Prompt: "p"},
	})
	if err == nil {
		t.Fatal("给不存在的用户落库居然成功了")
	}
	if !strings.Contains(err.Error(), "create asset") {
		t.Errorf("拦下它的应该是 INSERT assets 的外键，实际 err=%v", err)
	}
}
