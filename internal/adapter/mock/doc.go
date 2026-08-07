// Package mock implements a fake L0 driver that makes no upstream calls at all.
//
// # 它为什么存在
//
// **开发环境没有任何真实供应商凭证。** 没有 mock，整条链路就只能靠单测拼图，
// 提交 → 冻结积分 → worker 领取 → 状态机推进 → 产物转存 → 派生缩略图 →
// 记录血缘 → 结算扣费 → SSE 推送 这一串永远没被真正跑通过一次，
// 每个环节都"应该没问题"。
//
// # 它怎么做到有意义
//
// mock **走与真实驱动完全相同的路径**:
//
//   - 相同的 interface（实现 adapter.AsyncDriver，不是另开一条捷径）
//   - 相同的状态机（queued → running → succeeded，经过真实的 lease 领取与轮询）
//   - 相同的产物转存（asset.Transferor 照常下载、落存储、写我们自己的 storage key）
//   - 相同的派生与血缘（thumb_512 / poster 照常生成，lineage 照常记录）
//   - 相同的计费（照常 Hold → Charge，失败照常 Refund）
//
// **只有最底层那一次 HTTP 调用被替换**：不打上游，而是在本地生成一个占位文件，
// 并按 SimulatedDelay 模拟耗时，让轮询真的轮上几圈而不是瞬间返回。
//
// 这条边界是 mock 有没有价值的分水岭。如果 mock 顺手把转存、派生、记账也一并
// 跳过，它验证的就只有"mock 自己能跑"；只替换最底层 HTTP，验证的才是
// 除上游之外的全部代码。
//
// # 失败注入
//
// FailureRate 与 FailureCode 让 mock 能按比例产出失败任务。失败三分类的退款路径
// 与前端的三种失败呈现（红/黄/灰）如果没有可复现的触发方式，就只能靠等真实上游
// 出错来碰运气验证。
package mock

import (
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Name 是 Registry 的查找键。它同时是 domain.FamilyMock 与
// domain.VideoProtocolMock 的取值，二者字面量一致，因此配置成
// protocol_family=mock 或 video_protocol=mock 都能找到本驱动。
const Name = string(domain.FamilyMock)

// Family 是本驱动所属的协议族。
const Family = domain.FamilyMock

// 生成的占位产物的默认形态。占位文件由驱动在本地写出，
// 之后照常走 asset.Transferor 转存，与真实产物在 L3 眼里没有区别。
const (
	PlaceholderImageMIME = "image/png"
	PlaceholderVideoMIME = "video/mp4"
	// PlaceholderKeyPrefix 是占位文件在存储里的键前缀，便于清理时一眼认出。
	PlaceholderKeyPrefix = "mock/"
)

// DefaultSimulatedDelay 是模拟的上游生成耗时。取这个量级是为了让轮询真的轮上
// 几圈、让前端的乐观进度条真的动起来——设成 0 会让状态机在一次调用里跑完，
// 等于没验证轮询。
const DefaultSimulatedDelay = 8 * time.Second

// DefaultPollInterval 是本驱动的建议轮询间隔，取得比 SimulatedDelay 小，
// 保证一次任务至少被轮询若干次。
const DefaultPollInterval = 2 * time.Second

// statusMap 是 mock 内部状态到平台归一化状态的映射。
// 形状与三个真实驱动保持一致，是刻意的：mock 与真实驱动在 L2 眼里必须无差别。
var statusMap = map[string]adapter.Status{
	"queued":    adapter.StatusQueued,
	"running":   adapter.StatusRunning,
	"succeeded": adapter.StatusSucceeded,
	"failed":    adapter.StatusFailed,
	"canceled":  adapter.StatusCanceled,
}

// Driver 是本地假驱动的实例。它不持有 http.Client——不打网络正是它的定义。
type Driver struct {
	// SimulatedDelay 是模拟的生成耗时，零值时用 DefaultSimulatedDelay。
	SimulatedDelay time.Duration
	// PollInterval 覆盖 DefaultPollInterval，零值表示用默认。
	PollInterval time.Duration
	// FailureRate ∈ [0,1]，按比例让任务进入失败路径，用于验证退款与前端的失败呈现。
	FailureRate float64
	// FailureCode 是注入失败时使用的错误码，零值时用 internal_error。
	FailureCode domain.TaskErrorCode
	// PlaceholderRoot 是占位文件的落盘目录。
	PlaceholderRoot string
}
