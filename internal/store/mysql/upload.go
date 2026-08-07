package mysql

import (
	"context"
	"database/sql"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// uploadRepo 是 store.UploadRepo 的 MySQL 实现。
type uploadRepo struct{ db *sql.DB }

// NewUploadRepo 构造 store.UploadRepo。
func NewUploadRepo(db *sql.DB) store.UploadRepo { return &uploadRepo{db: db} }

const uploadColumns = `id, user_id, slot, storage_key, mime, bytes, width, height,
	expires_at, promoted_asset_id, created_at`

// scanUpload 读一行上传对象。
//
// PreviewURL 不落库：它是 /api/uploads/{id}/content 这种由 PublicBaseURL 拼出来的
// 传输层形态，与 Asset 的 Original 同理由 httpapi 层投影。把 URL 烧进库会让
// 换域名变成一次数据迁移。
func scanUpload(s rowScanner) (domain.Upload, error) {
	var (
		u        domain.Upload
		slot     sql.NullString
		width    sql.NullInt64
		height   sql.NullInt64
		assetID  sql.NullString
		expires  time.Time
		createdA time.Time
	)
	err := s.Scan(
		&u.ID, &u.UserID, &slot, &u.StorageKey, &u.MIME, &u.Bytes,
		&width, &height, &expires, &assetID, &createdA,
	)
	if err != nil {
		return domain.Upload{}, err
	}
	if slot.Valid {
		u.Slot = slot.String
	}
	u.Width = intPtr(width)
	u.Height = intPtr(height)
	u.ExpiresAt = expires.UTC()
	u.CreatedAt = createdA.UTC()
	u.AssetID = strPtr(assetID)
	return u, nil
}

func (r *uploadRepo) Create(ctx context.Context, u domain.Upload) (domain.Upload, error) {
	if err := requireID("upload user_id", u.UserID); err != nil {
		return domain.Upload{}, err
	}
	if u.StorageKey == "" {
		return domain.Upload{}, invalidParam("upload storage_key must not be empty")
	}
	if u.MIME == "" {
		return domain.Upload{}, invalidParam("upload mime must not be empty")
	}
	if u.ID == "" {
		u.ID = uid.New()
	}
	// upload 是 24h 回收的临时对象，过期时刻是它生命周期的定义本身。
	// 调用方没给就补一个默认值，落一个零值时间会让 reaper 立刻把它删掉。
	if u.ExpiresAt.IsZero() {
		u.ExpiresAt = time.Now().UTC().Add(24 * time.Hour)
	}
	createdAt := u.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO uploads
		 (id, user_id, slot, storage_key, mime, bytes, width, height, status, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		u.ID, u.UserID, nullStringNonEmpty(u.Slot), u.StorageKey, u.MIME, u.Bytes,
		nullInt(u.Width), nullInt(u.Height), u.ExpiresAt.UTC(), createdAt.UTC())
	if err != nil {
		return domain.Upload{}, wrap("create upload", err)
	}
	return r.Get(ctx, u.ID)
}

func (r *uploadRepo) Get(ctx context.Context, id string) (domain.Upload, error) {
	if err := requireID("upload id", id); err != nil {
		return domain.Upload{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE id = ?`, id)
	u, err := scanUpload(row)
	if err != nil {
		return domain.Upload{}, wrapScan("upload", id, "get upload", err)
	}
	return u, nil
}

// MarkPromoted 把一个 upload 标记为已提升为 asset。
//
// 状态与 promoted_asset_id 一起写，且只对仍 pending 的行生效：一个 upload
// 被两条并发任务同时引用时，只有先到的那次提升算数，后到的看到 0 行——
// 否则 promoted_asset_id 会被后一个 asset 覆盖，前一个 asset 的输入素材
// 就在回收扫描里失去了保护。
func (r *uploadRepo) MarkPromoted(ctx context.Context, id, assetID string) error {
	if err := requireID("upload id", id); err != nil {
		return err
	}
	if err := requireID("asset id", assetID); err != nil {
		return err
	}

	res, err := r.db.ExecContext(ctx,
		`UPDATE uploads SET status = 'promoted', promoted_asset_id = ?
		 WHERE id = ? AND status = 'pending'`, assetID, id)
	if err != nil {
		return wrap("mark upload promoted", err)
	}
	n, err := affected(res, "mark upload promoted")
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}

	var (
		status   string
		existing sql.NullString
	)
	switch err := r.db.QueryRowContext(ctx,
		`SELECT status, promoted_asset_id FROM uploads WHERE id = ?`, id).Scan(&status, &existing); {
	case isNoRows(err):
		return notFound("upload", id)
	case err != nil:
		return wrap("read upload status", err)
	}
	// 幂等：已经提升成同一个 asset，当作成功。重复引用同一个 upload 是常态。
	if existing.Valid && existing.String == assetID {
		return nil
	}
	return conflict("upload "+id+" is already "+status, nil)
}

// ListExpired 供回收任务扫描已过期且仍未被提升的上传。
//
// 已提升的（status='promoted'）不在其列：它的字节已经属于某个 asset，
// 删掉会让正在被血缘引用的输入素材凭空消失。这条过滤是 uploads 与 assets
// 分表的直接理由——两者的生命周期语义不同。
func (r *uploadRepo) ListExpired(ctx context.Context, before time.Time, limit int) ([]domain.Upload, error) {
	limit = clampLimit(limit)
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+uploadColumns+` FROM uploads
		 WHERE status = 'pending' AND promoted_asset_id IS NULL AND expires_at < ?
		 ORDER BY expires_at ASC LIMIT ?`,
		before.UTC(), limit)
	if err != nil {
		return nil, wrap("list expired uploads", err)
	}
	defer rows.Close()

	var out []domain.Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, wrap("scan upload", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list expired uploads", err)
	}
	return out, nil
}

// Delete 物理删除一条 upload 记录。
//
// upload 是临时对象，删它不影响血缘（血缘挂在 assets 上），因此这里是真删
// 而不是像 AssetRepo.Delete 那样软删。
func (r *uploadRepo) Delete(ctx context.Context, id string) error {
	if err := requireID("upload id", id); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM uploads WHERE id = ?`, id)
	if err != nil {
		return wrap("delete upload", err)
	}
	n, err := affected(res, "delete upload")
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound("upload", id)
	}
	return nil
}
