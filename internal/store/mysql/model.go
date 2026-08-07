package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// modelRepo 是 store.ModelRepo 的 MySQL 实现。
//
// **本类型刻意不带任何进程内缓存。** 模型池每周在变，管理员加一条模型记录后
// 必须立刻在 GET /api/models 里可见（"接新模型不发版"这条设计线的兑现点）。
// 缓存要放也该放在上层那个按 config_versions 版本号整表重载的 ModelCache 里，
// 由它决定失效时机；仓储自己再藏一层缓存会让那个版本号失去意义。
type modelRepo struct{ db *sql.DB }

// NewModelRepo 构造 store.ModelRepo。
func NewModelRepo(db *sql.DB) store.ModelRepo { return &modelRepo{db: db} }

const modelColumns = `id, provider_id, upstream_model, protocol_family, video_protocol,
	capability, request_mapping, poll_interval_seconds, enabled, updated_at`

func scanModel(s rowScanner) (domain.ModelConfig, error) {
	var (
		m              domain.ModelConfig
		videoProtocol  sql.NullString
		capabilityRaw  []byte
		mappingRaw     []byte
		pollIntervalNS sql.NullInt64
	)
	err := s.Scan(
		&m.ID, &m.ProviderID, &m.UpstreamModel, &m.Family, &videoProtocol,
		&capabilityRaw, &mappingRaw, &pollIntervalNS, &m.Enabled, &m.UpdatedAt,
	)
	if err != nil {
		return domain.ModelConfig{}, err
	}
	if videoProtocol.Valid {
		vp := domain.VideoProtocol(videoProtocol.String)
		m.VideoProtocol = &vp
	}
	if m.Capability, err = scanJSONAny(capabilityRaw); err != nil {
		return domain.ModelConfig{}, err
	}
	if m.RequestMapping, err = scanJSONAny(mappingRaw); err != nil {
		return domain.ModelConfig{}, err
	}
	m.PollIntervalSeconds = intPtr(pollIntervalNS)
	m.UpdatedAt = m.UpdatedAt.UTC()
	return m, nil
}

// List 每次都查库，见类型注释。
//
// 排序与 idx_models_modality_enabled_order 对齐：(modality, enabled, display_order)。
// display_order 相同的按 id 兜底，否则两次请求返回的顺序可能不一样，
// 前端的模型选择器就会莫名其妙地跳。
func (r *modelRepo) List(ctx context.Context, f domain.ModelFilter) ([]domain.ModelConfig, error) {
	where := []string{}
	args := []any{}
	if f.Modality != "" {
		where = append(where, `modality = ?`)
		args = append(args, string(f.Modality))
	}
	if f.Enabled != nil {
		where = append(where, `enabled = ?`)
		args = append(args, *f.Enabled)
	}

	sqlText := `SELECT ` + modelColumns + ` FROM models`
	if len(where) > 0 {
		sqlText += ` WHERE ` + strings.Join(where, ` AND `)
	}
	sqlText += ` ORDER BY display_order ASC, id ASC`

	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, wrap("list models", err)
	}
	defer rows.Close()

	var out []domain.ModelConfig
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, wrap("scan model", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list models", err)
	}
	return out, nil
}

func (r *modelRepo) Get(ctx context.Context, id string) (domain.ModelConfig, error) {
	if err := requireID("model id", id); err != nil {
		return domain.ModelConfig{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM models WHERE id = ?`, id)
	m, err := scanModel(row)
	if err != nil {
		return domain.ModelConfig{}, wrapScan("model", id, "get model", err)
	}
	return m, nil
}

// capabilityFacets 是 capability JSON 里需要被提升成独立列的那几个字段。
//
// 它们在 capability 里已经有一份（那份要逐字下发前端），列里再存一份是为了
// 能建索引：GET /api/models?modality=image 是最热的读之一，
// 走 JSON_EXTRACT 过滤等于每次全表扫。两份数据的一致性由 Upsert 保证——
// 列永远从 capability 派生，不接受调用方单独指定，也就不存在两份对不上的可能。
type capabilityFacets struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Vendor      string          `json:"vendor"`
	Modality    domain.Modality `json:"modality"`
	Order       int             `json:"order"`
	Description string          `json:"description"`
	PreviewURL  string          `json:"preview_url"`
}

// extractFacets 从 capability 里取出要落成独立列的字段。
func extractFacets(capability any) (capabilityFacets, []byte, error) {
	if capability == nil {
		return capabilityFacets{}, nil, invalidParam("model capability must not be empty")
	}
	raw, err := json.Marshal(capability)
	if err != nil {
		return capabilityFacets{}, nil, invalidParam("model capability is not valid JSON: " + err.Error())
	}
	var f capabilityFacets
	if err := json.Unmarshal(raw, &f); err != nil {
		return capabilityFacets{}, nil, invalidParam("model capability is not an object: " + err.Error())
	}
	return f, raw, nil
}

// Upsert 新增或整体覆盖一条模型配置，并在同一事务里推配置版本号。
func (r *modelRepo) Upsert(ctx context.Context, m domain.ModelConfig) (domain.ModelConfig, error) {
	facets, capabilityRaw, err := extractFacets(m.Capability)
	if err != nil {
		return domain.ModelConfig{}, err
	}

	if m.ID == "" {
		// 模型 id 是人类可读 slug（会原样出现在 API 与前端表单里），
		// 不生成 UUID 兜底；capability.id 是它唯一合法的来源。
		m.ID = facets.ID
	}
	if m.ID == "" {
		return domain.ModelConfig{}, invalidParam("model id must not be empty")
	}
	// capability.id 会原样下发前端、再原样回传到 POST /api/tasks，
	// 与行主键不一致时提交上来的任务会查不到模型。
	if facets.ID != "" && facets.ID != m.ID {
		return domain.ModelConfig{}, invalidParam(
			"capability.id " + facets.ID + " does not match model id " + m.ID)
	}
	if err := requireID("model provider_id", m.ProviderID); err != nil {
		return domain.ModelConfig{}, err
	}
	switch m.Family {
	case domain.FamilyChat, domain.FamilyImages, domain.FamilyVideo, domain.FamilyMock:
	default:
		return domain.ModelConfig{}, invalidParam("unknown protocol family " + string(m.Family))
	}
	if m.Family == domain.FamilyVideo && m.VideoProtocol == nil {
		return domain.ModelConfig{}, invalidParam("video models require a video_protocol")
	}
	switch facets.Modality {
	case domain.ModalityImage, domain.ModalityVideo:
	default:
		return domain.ModelConfig{}, invalidParam("capability.modality must be image or video")
	}
	if facets.Name == "" || facets.Vendor == "" {
		return domain.ModelConfig{}, invalidParam("capability.name and capability.vendor are required")
	}
	if strings.TrimSpace(m.UpstreamModel) == "" {
		return domain.ModelConfig{}, invalidParam("model upstream_model must not be empty")
	}

	var mappingArg any
	if mappingArg, err = jsonValue(m.RequestMapping); err != nil {
		return domain.ModelConfig{}, invalidParam("model request_mapping is not valid JSON: " + err.Error())
	}

	var videoProtocol any
	if m.VideoProtocol != nil {
		videoProtocol = string(*m.VideoProtocol)
	}

	const q = `INSERT INTO models
		(id, provider_id, upstream_model, protocol_family, video_protocol, modality,
		 enabled, display_order, name, vendor, description, preview_url,
		 capability, request_mapping, poll_interval_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  provider_id           = VALUES(provider_id),
		  upstream_model        = VALUES(upstream_model),
		  protocol_family       = VALUES(protocol_family),
		  video_protocol        = VALUES(video_protocol),
		  modality              = VALUES(modality),
		  enabled               = VALUES(enabled),
		  display_order         = VALUES(display_order),
		  name                  = VALUES(name),
		  vendor                = VALUES(vendor),
		  description           = VALUES(description),
		  preview_url           = VALUES(preview_url),
		  capability            = VALUES(capability),
		  request_mapping       = VALUES(request_mapping),
		  poll_interval_seconds = VALUES(poll_interval_seconds)`

	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, q,
			m.ID, m.ProviderID, m.UpstreamModel, string(m.Family), videoProtocol,
			string(facets.Modality), m.Enabled, facets.Order, facets.Name, facets.Vendor,
			nullStringNonEmpty(facets.Description), nullStringNonEmpty(facets.PreviewURL),
			capabilityRaw, mappingArg, nullInt(m.PollIntervalSeconds)); err != nil {
			return wrap("upsert model", err)
		}
		return bumpConfigVersion(ctx, tx, "model upsert: "+m.ID)
	})
	if err != nil {
		return domain.ModelConfig{}, err
	}
	return r.Get(ctx, m.ID)
}

// Delete 删除一条模型配置。
//
// tasks.model_id 上是 ON DELETE RESTRICT，因此已经跑过任务的模型删不掉，
// 驱动报 1451 由 wrap 翻成 409。这是对的：历史任务要能回显它当时用的模型，
// 想下架就把 enabled 置 0，而不是抹掉记录。
func (r *modelRepo) Delete(ctx context.Context, id string) error {
	if err := requireID("model id", id); err != nil {
		return err
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
		if err != nil {
			return wrap("delete model", err)
		}
		n, err := affected(res, "delete model")
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("model", id)
		}
		return bumpConfigVersion(ctx, tx, "model delete: "+id)
	})
}

// Fingerprint 返回模型配置的版本指纹，直接作为 GET /api/models 的 ETag。
//
// 返回值**已经是带引号的强 ETag 形态**（形如 "v1-3f2a…"），调用方原样
// 塞进响应头即可，不要再包一层引号——RFC 9110 的 ETag 必须带引号，
// 而"由谁来加这对引号"如果不定死，两边都不加或都加是同样容易发生的事故。
//
// 指纹的主来源是 config_versions 的 MAX(id)：providers / models / skills 的
// 每次变更都会推一行，因此它一变就说明配置动过了。同时折进 models 表的
// 聚合（条数 + 最新 updated_at），是为了兜住绕过本仓储、直接写库的路径
// （迁移里的 seed、DBA 手改）——那种情况下版本号没被推，但模型池确实变了，
// 只认 config_versions 会让前端一直拿 304 看到旧列表。
//
// 两个来源都是一次索引/主键上的聚合查询，没有进程内缓存：加一条模型记录
// 必须立刻改变指纹，任何缓存都会把"不重启即生效"退化成"下次重载才生效"。
func (r *modelRepo) Fingerprint(ctx context.Context) (string, error) {
	const q = `SELECT
		(SELECT COALESCE(MAX(id), 0) FROM config_versions),
		(SELECT COUNT(*) FROM models WHERE enabled = 1),
		(SELECT COALESCE(MAX(UNIX_TIMESTAMP(updated_at) * 1000), 0) FROM models WHERE enabled = 1)`

	var (
		configVersion int64
		enabledCount  int64
		latestUpdate  float64
	)
	if err := r.db.QueryRowContext(ctx, q).Scan(&configVersion, &enabledCount, &latestUpdate); err != nil {
		return "", wrap("model fingerprint", err)
	}

	seed := strconv.FormatInt(configVersion, 10) + ":" +
		strconv.FormatInt(enabledCount, 10) + ":" +
		strconv.FormatInt(int64(latestUpdate), 10)
	sum := sha256.Sum256([]byte(seed))
	// 截到 16 字节：ETag 只需要"变了能看出来"，128 位的碰撞概率远低于
	// 一次网络抖动，而完整 64 位十六进制串每个响应头都要多背 32 字节。
	return `"v1-` + hex.EncodeToString(sum[:16]) + `"`, nil
}
