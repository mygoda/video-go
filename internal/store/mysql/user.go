package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// userRepo 是 store.UserRepo 的 MySQL 实现。
type userRepo struct{ db *sql.DB }

// NewUserRepo 构造 store.UserRepo。
func NewUserRepo(db *sql.DB) store.UserRepo { return &userRepo{db: db} }

// userColumns 是用户的完整投影。
//
// task_count 用子查询而不是在 users 上再加一个计数列：那个列每提交一个任务
// 就要更新一次，而它只在管理端列表里被读到，为一个低频读付高频写不划算。
const userColumns = `u.id, u.username, u.password_hash, u.role, u.status,
	u.credits, u.credits_held, u.storage_used_bytes, u.storage_quota_bytes,
	u.last_active_at, u.created_at,
	(SELECT COUNT(*) FROM tasks t WHERE t.user_id = u.id)`

func scanUser(s rowScanner) (domain.User, error) {
	var (
		u            domain.User
		lastActiveAt sql.NullTime
	)
	err := s.Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status,
		&u.Credits, &u.CreditsHeld, &u.StorageUsed, &u.StorageQuota,
		&lastActiveAt, &u.CreatedAt, &u.TaskCount,
	)
	if err != nil {
		return domain.User{}, err
	}
	u.CreatedAt = u.CreatedAt.UTC()
	u.LastActiveAt = timePtr(lastActiveAt)
	return u, nil
}

func (r *userRepo) Create(ctx context.Context, u domain.User) (domain.User, error) {
	if strings.TrimSpace(u.Username) == "" {
		return domain.User{}, invalidParam("username must not be empty")
	}
	if u.ID == "" {
		u.ID = uid.New()
	}
	if u.Role == "" {
		u.Role = domain.RoleUser
	}
	if u.Status == "" {
		u.Status = domain.UserStatusActive
	}

	const q = `INSERT INTO users
		(id, username, password_hash, role, status, credits, credits_held,
		 storage_used_bytes, storage_quota_bytes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	createdAt := u.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	// storage_quota_bytes 为 0 时走列默认值，而不是把用户配额真的设成 0：
	// 调用方不填这个字段的意思是"用默认配额"，不是"一个字节都不许存"。
	quota := u.StorageQuota
	if quota <= 0 {
		const defaultQuota = 10737418240
		quota = defaultQuota
	}

	_, err := r.db.ExecContext(ctx, q,
		u.ID, u.Username, u.PasswordHash, u.Role, u.Status,
		u.Credits, u.CreditsHeld, u.StorageUsed, quota, createdAt.UTC())
	if err != nil {
		if isDup(err) {
			return domain.User{}, conflict("username "+u.Username+" already exists", err)
		}
		return domain.User{}, wrap("create user", err)
	}
	return r.GetByID(ctx, u.ID)
}

func (r *userRepo) GetByID(ctx context.Context, id string) (domain.User, error) {
	if err := requireID("user id", id); err != nil {
		return domain.User{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, wrapScan("user", id, "get user", err)
	}
	return u, nil
}

func (r *userRepo) GetByUsername(ctx context.Context, username string) (domain.User, error) {
	if err := requireID("username", username); err != nil {
		return domain.User{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.username = ?`, username)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, wrapScan("user", username, "get user by username", err)
	}
	return u, nil
}

func (r *userRepo) UpdatePassword(ctx context.Context, id, passwordHash string) error {
	if err := requireID("user id", id); err != nil {
		return err
	}
	if passwordHash == "" {
		return invalidParam("password hash must not be empty")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return wrap("update password", err)
	}
	// 影响 0 行有两种可能：用户不存在，或新哈希与旧哈希逐字节相同。
	// 后者在 bcrypt/argon2 下不可能（每次加盐都不同），因此判定为不存在。
	n, err := affected(res, "update password")
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound("user", id)
	}
	return nil
}

func (r *userRepo) UpdateProfile(ctx context.Context, id string, role domain.Role, status domain.UserStatus, storageQuota int64) (domain.User, error) {
	if err := requireID("user id", id); err != nil {
		return domain.User{}, err
	}
	if role != domain.RoleUser && role != domain.RoleAdmin {
		return domain.User{}, invalidParam("unknown role " + string(role))
	}
	if status != domain.UserStatusActive && status != domain.UserStatusDisabled {
		return domain.User{}, invalidParam("unknown status " + string(status))
	}
	if storageQuota < 0 {
		return domain.User{}, invalidParam("storage quota must not be negative")
	}

	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET role = ?, status = ?, storage_quota_bytes = ? WHERE id = ?`,
		role, status, storageQuota, id)
	if err != nil {
		return domain.User{}, wrap("update profile", err)
	}
	// 这里不拿 RowsAffected 判存在：把角色改成它已有的值时 MySQL 报 0 行，
	// 那不是"用户不存在"。存在性交给紧接着的读来判。
	_ = res
	return r.GetByID(ctx, id)
}

// List 按 (created_at DESC, id DESC) 游标分页，q 非空时按用户名模糊过滤。
//
// LIKE 的模式串是绑定参数，通配符由这里拼进**参数值**而不是 SQL 文本，
// 因此用户输入里的 % 与 _ 只会被当成通配符，不会改变语句结构。
func (r *userRepo) List(ctx context.Context, q, cursor string, limit int) (store.Page[domain.User], error) {
	limit = clampLimit(limit)

	where := []string{}
	args := []any{}
	if s := strings.TrimSpace(q); s != "" {
		where = append(where, `u.username LIKE ?`)
		args = append(args, "%"+s+"%")
	}
	if c, ok := decodeCursor(cursor); ok {
		where = append(where, `(u.created_at < ? OR (u.created_at = ? AND u.id < ?))`)
		args = append(args, c.At, c.At, c.ID)
	}

	sqlText := `SELECT ` + userColumns + ` FROM users u`
	if len(where) > 0 {
		sqlText += ` WHERE ` + strings.Join(where, ` AND `)
	}
	sqlText += ` ORDER BY u.created_at DESC, u.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return store.Page[domain.User]{}, wrap("list users", err)
	}
	defer rows.Close()

	var items []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return store.Page[domain.User]{}, wrap("scan user", err)
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil {
		return store.Page[domain.User]{}, wrap("list users", err)
	}

	items, hasMore := pageSlice(items, limit)
	page := store.Page[domain.User]{Items: items}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		page.NextCursor = encodeTimeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

func (r *userRepo) TouchLastActive(ctx context.Context, id string, at time.Time) error {
	if err := requireID("user id", id); err != nil {
		return err
	}
	// 只在新时间更晚时才写：并发请求乱序到达时，一个慢请求不该把
	// last_active_at 往回拨。
	_, err := r.db.ExecContext(ctx,
		`UPDATE users SET last_active_at = ?
		 WHERE id = ? AND (last_active_at IS NULL OR last_active_at < ?)`,
		at.UTC(), id, at.UTC())
	if err != nil {
		return wrap("touch last active", err)
	}
	return nil
}
