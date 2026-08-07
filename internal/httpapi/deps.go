package httpapi

import (
	"context"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/canvas"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/executor"
	"github.com/aigc-pool/aigc-pool/internal/skill"
	"github.com/aigc-pool/aigc-pool/internal/store"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

// TokenIssuer 签发与校验访问令牌。
//
// 声明成接口而不是直接依赖某个 JWT 库，有两个原因:
// 一是 handler 只需要"签一个 / 验一个"这两件事，不需要知道底下是 JWT 还是别的；
// 二是测试里可以塞一个固定令牌的假实现，不必为了跑一个 handler 测试
// 去构造真实签名。
//
// 密钥从 env 读（见 config.Config.JWTSecret），不入库、不进代码。
type TokenIssuer interface {
	Issue(userID string, role domain.Role, ttl time.Duration) (token string, expiresAt time.Time, err error)
	Verify(token string) (userID string, role domain.Role, err error)
}

// PasswordHasher 处理密码哈希与校验。
//
// 同样声明成接口：算法（bcrypt / argon2）是可替换的实现细节，
// 而"存哈希不存明文、比对走恒定时间函数"这两条是接口层面的约定。
//
// Needs Rehash 用于算法参数升级后的平滑迁移：用户下次成功登录时
// 用新参数重算一遍，不需要强制全员改密码。
type PasswordHasher interface {
	Hash(plain string) (string, error)
	Compare(hash, plain string) error
	NeedsRehash(hash string) bool
}

// Clock 抽象当前时间，让涉及过期、租约、配额窗口的 handler 可测。
type Clock interface {
	Now() time.Time
}

// Deps 是 handler 需要的全部依赖。
//
// 它把"这一层会碰到什么"摊开写在一个地方：读到这个结构体就知道 HTTP 层
// 依赖了哪些能力，反过来也能看出哪些能力**不**在这里——比如没有任何
// adapter driver 的具体类型，只有 adapter.Registry，这正是"协议差异不外泄"
// 在依赖清单上的体现。
//
// 全部字段都是接口（除 Config），因此 handler 测试可以逐个替换成假实现，
// 不需要起数据库、不需要真实凭证。
type Deps struct {
	Config config.Config

	// ── 认证 ─────────────────────────────────────────────────────
	Tokens TokenIssuer
	Hasher PasswordHasher
	Clock  Clock

	// ── 存储（每聚合一个仓储）───────────────────────────────────
	Users     store.UserRepo
	Providers store.ProviderRepo
	Models    store.ModelRepo
	Tasks     store.TaskRepo
	Events    store.EventRepo
	Uploads   store.UploadRepo
	Assets    store.AssetRepo
	Ledgers   store.LedgerRepo
	Canvases  store.CanvasRepo
	Skills    store.SkillRepo

	// ── L1 能力声明 ──────────────────────────────────────────────
	Evaluator capability.Evaluator
	Pricer    capability.Pricer
	Validator capability.Validator

	// ── L0 协议适配 ──────────────────────────────────────────────
	// 只有 Registry，没有任何具体 driver 类型。
	Drivers adapter.Registry

	// ── L2 执行 ──────────────────────────────────────────────────
	Queue    executor.Queue
	Runner   executor.Runner
	Poller   executor.PollScheduler
	Breakers executor.CircuitBreaker

	// ── L3 资产 ──────────────────────────────────────────────────
	Blobs      asset.Store
	Transferor asset.Transferor
	Deriver    asset.Deriver
	Lineage    asset.LineageRecorder
	Quota      asset.QuotaGuard

	// ── 其他业务能力 ─────────────────────────────────────────────
	Ledger billing.Ledger
	Canvas canvas.Applier
	Skill  skill.Repository
	Stream stream.Hub
}

// Identity 是经过认证的调用方身份，由 Auth 中间件放进 context。
type Identity struct {
	UserID string
	Role   domain.Role
}

// IsAdmin 报告该身份是否为管理员。AdminOnly 中间件据此放行 /api/admin/*。
func (i Identity) IsAdmin() bool { return i.Role == domain.RoleAdmin }

// identityKey 是 Identity 在 context 里的键类型。用私有类型而不是字符串，
// 避免与其他包的 context 键碰撞。
type identityKey struct{}

// WithIdentity 把身份放进 context，由 Auth 中间件调用。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom 取出 context 里的身份。第二个返回值为 false 表示未认证。
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}
