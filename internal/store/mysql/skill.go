package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// skillRepo 是 store.SkillRepo 的 MySQL 实现。
//
// 与 modelRepo 同理：不带进程内缓存。Skill 与 providers / models 同属
// config_versions 管的那一族配置，"加一条记录就多一个玩法"要立刻生效，
// 缓存放在上层按版本号整表重载的地方。
type skillRepo struct{ db *sql.DB }

// NewSkillRepo 构造 store.SkillRepo。
func NewSkillRepo(db *sql.DB) store.SkillRepo { return &skillRepo{db: db} }

const skillColumns = `id, name, description, system_prompt, default_model_id,
	default_params, enabled, display_order`

// promptRef 由 id 与提示词原文算出一个不可逆的标识。
//
// 库里没有 system_prompt_ref 这一列，而它又必须存在（用户端只拿得到这个
// 标识，原文是资产）。派生而不是新加一列的理由：它不承载任何原文之外的
// 信息，存一份就多一份能与原文对不上的状态。折进内容哈希而不是只用 id，
// 是为了让管理员改了提示词之后标识跟着变——前端据此就能知道
// "同一个 Skill 的提示词换过了"，而这恰恰是缓存该失效的时刻。
func promptRef(id, systemPrompt string) string {
	if systemPrompt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id + "\x00" + systemPrompt))
	return "sp_" + hex.EncodeToString(sum[:8])
}

func scanSkill(s rowScanner) (domain.Skill, error) {
	var (
		sk           domain.Skill
		description  sql.NullString
		defaultModel sql.NullString
		paramsRaw    []byte
	)
	err := s.Scan(
		&sk.ID, &sk.Name, &description, &sk.SystemPrompt, &defaultModel,
		&paramsRaw, &sk.Enabled, &sk.Order,
	)
	if err != nil {
		return domain.Skill{}, err
	}
	if description.Valid {
		sk.Description = description.String
	}
	sk.DefaultModelID = strPtr(defaultModel)
	if err := scanJSON(paramsRaw, &sk.DefaultParams); err != nil {
		return domain.Skill{}, err
	}
	if sk.DefaultParams == nil {
		// default_params 在 API 里是 required：给一个空对象而不是 null，
		// 前端就不必对 `params ?? {}` 这种写法层层设防。
		sk.DefaultParams = map[string]any{}
	}
	sk.SystemPromptRef = promptRef(sk.ID, sk.SystemPrompt)
	return sk, nil
}

// List 返回 Skill 列表。
//
// includeDisabled 为 false 时只给启用的，那是用户端 GET /api/skills 的形态；
// 管理端传 true 才看得到已禁用的记录。
//
// 排序与 idx_skills_enabled_order (enabled, display_order) 对齐，
// display_order 相同的按 id 兜底，避免两次请求顺序不一致让选择器跳动。
func (r *skillRepo) List(ctx context.Context, includeDisabled bool) ([]domain.Skill, error) {
	// 两支都是常量 SQL：唯一的差异是要不要那半句 WHERE，
	// 拼一个变量进去只会让"这条 SQL 到底长什么样"要读代码才知道。
	const listEnabled = `SELECT ` + skillColumns + ` FROM skills WHERE enabled = 1
		ORDER BY display_order ASC, id ASC`
	const listAll = `SELECT ` + skillColumns + ` FROM skills
		ORDER BY display_order ASC, id ASC`

	q := listEnabled
	if includeDisabled {
		q = listAll
	}

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, wrap("list skills", err)
	}
	defer rows.Close()

	var out []domain.Skill
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, wrap("scan skill", err)
		}
		out = append(out, sk)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("list skills", err)
	}
	return out, nil
}

// Get 按 id 读一条，**不过滤 enabled**。
//
// 已禁用的 Skill 仍要读得到：管理端要能打开它改回来，历史画布消息上的
// skill_id 也要能显示当时用的是哪个（那是个软引用，见迁移文件的注释）。
func (r *skillRepo) Get(ctx context.Context, id string) (domain.Skill, error) {
	if err := requireID("skill id", id); err != nil {
		return domain.Skill{}, err
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+skillColumns+` FROM skills WHERE id = ?`, id)
	sk, err := scanSkill(row)
	if err != nil {
		return domain.Skill{}, wrapScan("skill", id, "get skill", err)
	}
	return sk, nil
}

// Upsert 新增或整体覆盖一条 Skill，并在同一事务里推配置版本号。
//
// Skill 与 providers / models 同属 config_versions 管的那一族配置，
// 漏推版本号的后果是"管理员改完了但在跑的实例看不见"（见 bumpConfigVersion）。
//
// default_model_id 是软引用，这里不校验模型是否存在：模型可以被管理员删掉，
// 那时 Skill 回落到默认模型，而不是变成一条插不进去的记录。
func (r *skillRepo) Upsert(ctx context.Context, s domain.Skill) (domain.Skill, error) {
	if strings.TrimSpace(s.Name) == "" {
		return domain.Skill{}, invalidParam("skill name must not be empty")
	}
	if strings.TrimSpace(s.SystemPrompt) == "" {
		// system_prompt 是 Skill 存在的全部理由：没有它这条记录与
		// "直接选个模型自己写提示词"没有区别，落库只会变成一条空壳。
		return domain.Skill{}, invalidParam("skill system_prompt must not be empty")
	}
	if s.ID == "" {
		s.ID = uid.New()
	}

	paramsArg, err := jsonValue(s.DefaultParams)
	if err != nil {
		return domain.Skill{}, invalidParam("skill default_params is not valid JSON: " + err.Error())
	}

	const q = `INSERT INTO skills
		(id, name, description, system_prompt, default_model_id,
		 default_params, enabled, display_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  name             = VALUES(name),
		  description      = VALUES(description),
		  system_prompt    = VALUES(system_prompt),
		  default_model_id = VALUES(default_model_id),
		  default_params   = VALUES(default_params),
		  enabled          = VALUES(enabled),
		  display_order    = VALUES(display_order)`

	err = withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, q,
			s.ID, s.Name, nullStringNonEmpty(s.Description), s.SystemPrompt,
			nullString(s.DefaultModelID), paramsArg, s.Enabled, s.Order); err != nil {
			return wrap("upsert skill", err)
		}
		return bumpConfigVersion(ctx, tx, "skill upsert: "+s.ID)
	})
	if err != nil {
		return domain.Skill{}, err
	}
	return r.Get(ctx, s.ID)
}

// Delete 删除一条 Skill。
//
// 真删而不是软删：canvas_messages.skill_id 与 tasks 的参数快照都是软引用
// （不加外键，见迁移文件），删掉不会留下悬挂的外键，历史记录里那个 id
// 只是查不到对应记录而已，前端本来就要处理这种情况。
// 想临时下架用 enabled = 0，那才是可逆的操作。
func (r *skillRepo) Delete(ctx context.Context, id string) error {
	if err := requireID("skill id", id); err != nil {
		return err
	}
	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM skills WHERE id = ?`, id)
		if err != nil {
			return wrap("delete skill", err)
		}
		n, err := affected(res, "delete skill")
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("skill", id)
		}
		return bumpConfigVersion(ctx, tx, "skill delete: "+id)
	})
}
