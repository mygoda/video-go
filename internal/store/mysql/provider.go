package mysql

import (
	"context"
	"database/sql"
	"os"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// providerRepo 是 store.ProviderRepo 的 MySQL 实现。
type providerRepo struct{ db *sql.DB }

// NewProviderRepo 构造 store.ProviderRepo。
func NewProviderRepo(db *sql.DB) store.ProviderRepo { return &providerRepo{db: db} }

const providerColumns = `id, name, base_url, credential_ref, auth_style, auth_header_name,
	enabled, timeout_ms, max_concurrency, created_at`

// scanProvider 读一行供应商。
//
// CredentialPresent 由 credential_ref 指向的**环境变量当前是否有值**算出来，
// 不是库里的列：密钥本身不入库，库里只有变量名，因此"配没配"这件事只能
// 在读的这一刻问一次环境。
func scanProvider(s rowScanner) (domain.Provider, error) {
	var (
		p              domain.Provider
		authHeaderName sql.NullString
	)
	err := s.Scan(
		&p.ID, &p.Name, &p.BaseURL, &p.CredentialRef, &p.AuthStyle, &authHeaderName,
		&p.Enabled, &p.TimeoutMS, &p.MaxConcurrency, &p.CreatedAt,
	)
	if err != nil {
		return domain.Provider{}, err
	}
	if authHeaderName.Valid {
		p.AuthHeaderName = authHeaderName.String
	}
	p.CreatedAt = p.CreatedAt.UTC()
	p.CredentialPresent = strings.TrimSpace(os.Getenv(p.CredentialRef)) != ""
	return p, nil
}

func (r *providerRepo) List(ctx context.Context) ([]domain.Provider, error) {
	// 供应商是几条到几十条的配置数据，一次全量返回；这里没有游标分页，
	// 因为管理端需要的是"全部供应商"这一份完整视图。
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+providerColumns+` FROM providers ORDER BY name ASC`)
	if err != nil {
		return nil, wrap("list providers", err)
	}
	defer rows.Close()

	var out []domain.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, wrap("scan provider", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list providers", err)
	}
	return out, nil
}

func (r *providerRepo) Get(ctx context.Context, id string) (domain.Provider, error) {
	if err := requireID("provider id", id); err != nil {
		return domain.Provider{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+providerColumns+` FROM providers WHERE id = ?`, id)
	p, err := scanProvider(row)
	if err != nil {
		return domain.Provider{}, wrapScan("provider", id, "get provider", err)
	}
	return p, nil
}

// Upsert 新增或整体覆盖一条供应商配置，并在同一事务里推配置版本号。
//
// 落库前先按 name 加锁查一次重名：providers 上除主键外还有
// uq_providers_name，而 ON DUPLICATE KEY UPDATE 对**任意**唯一键命中都会转成
// UPDATE。只靠它的话，拿一个新 ID 配上已被占用的 name 提交，会把那个同名的
// 旧供应商整行改写成新配置——ID 还是旧的，管理员看到的是"我新建的供应商
// 没出现，另一家的配置却被换掉了"，而且全程没有任何报错。预检把这种情况
// 变成 409。
//
// FOR UPDATE 是必须的：非锁定读挡不住并发的同名插入，两个事务都通过预检后
// 后提交的那个仍会走进 ON DUPLICATE 分支静默覆盖。锁住唯一索引上那条记录后，
// 并发方会阻塞到本事务提交，再读时就看得见已存在的行。
func (r *providerRepo) Upsert(ctx context.Context, p domain.Provider) (domain.Provider, error) {
	if strings.TrimSpace(p.Name) == "" {
		return domain.Provider{}, invalidParam("provider name must not be empty")
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return domain.Provider{}, invalidParam("provider base_url must not be empty")
	}
	if strings.TrimSpace(p.CredentialRef) == "" {
		return domain.Provider{}, invalidParam("provider credential_ref must not be empty")
	}
	if p.ID == "" {
		p.ID = uid.New()
	}
	if p.AuthStyle == "" {
		p.AuthStyle = domain.AuthStyleBearer
	}
	if p.TimeoutMS <= 0 {
		p.TimeoutMS = 30000
	}
	if p.MaxConcurrency <= 0 {
		p.MaxConcurrency = 8
	}

	const q = `INSERT INTO providers
		(id, name, base_url, credential_ref, auth_style, auth_header_name,
		 enabled, timeout_ms, max_concurrency)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  name             = VALUES(name),
		  base_url         = VALUES(base_url),
		  credential_ref   = VALUES(credential_ref),
		  auth_style       = VALUES(auth_style),
		  auth_header_name = VALUES(auth_header_name),
		  enabled          = VALUES(enabled),
		  timeout_ms       = VALUES(timeout_ms),
		  max_concurrency  = VALUES(max_concurrency)`

	err := withTx(ctx, r.db, func(tx *sql.Tx) error {
		var ownerID string
		switch err := tx.QueryRowContext(ctx,
			`SELECT id FROM providers WHERE name = ? FOR UPDATE`, p.Name).Scan(&ownerID); {
		case isNoRows(err):
			// 名字没被占，随便写。
		case err != nil:
			return wrap("check provider name", err)
		case ownerID != p.ID:
			return conflict("provider name "+p.Name+" already used by provider "+ownerID, nil)
		}

		if _, err := tx.ExecContext(ctx, q,
			p.ID, p.Name, p.BaseURL, p.CredentialRef, p.AuthStyle,
			nullStringNonEmpty(p.AuthHeaderName),
			p.Enabled, p.TimeoutMS, p.MaxConcurrency); err != nil {
			if isDup(err) {
				return conflict("provider name "+p.Name+" already used by another provider", err)
			}
			return wrap("upsert provider", err)
		}
		return bumpConfigVersion(ctx, tx, "provider upsert: "+p.ID)
	})
	if err != nil {
		return domain.Provider{}, err
	}
	return r.Get(ctx, p.ID)
}

// Delete 删除一条供应商配置。
//
// **仍被 models 引用时返回 409，绝不级联。** 级联删掉的模型对用户表现为
// "这个模型突然消失了"，而留下引用一个不存在供应商的模型则是一条
// 永远调用失败的死配置——两种结果都比让管理员先处理掉引用更糟。
// 这里显式先查引用再删，而不是只靠外键 RESTRICT 报错：外键给出的
// 是一句 MySQL 错误，管理员看不出到底是哪几个模型挡着。
func (r *providerRepo) Delete(ctx context.Context, id string) error {
	if err := requireID("provider id", id); err != nil {
		return err
	}

	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM models WHERE provider_id = ? ORDER BY id LIMIT 10`, id)
		if err != nil {
			return wrap("check provider references", err)
		}
		var refs []string
		for rows.Next() {
			var modelID string
			if err := rows.Scan(&modelID); err != nil {
				rows.Close()
				return wrap("scan provider references", err)
			}
			refs = append(refs, modelID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return wrap("check provider references", err)
		}
		rows.Close()

		if len(refs) > 0 {
			return conflict(
				"provider "+id+" is still referenced by models: "+strings.Join(refs, ", "), nil)
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
		if err != nil {
			return wrap("delete provider", err)
		}
		n, err := affected(res, "delete provider")
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("provider", id)
		}
		return bumpConfigVersion(ctx, tx, "provider delete: "+id)
	})
}
