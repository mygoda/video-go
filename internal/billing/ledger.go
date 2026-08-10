// Package billing owns the credit ledger.
//
// # 一句话规则
//
//	提交时 Hold（冻结）→ 成功时 Charge（结算）→ 失败时 Refund（释放）。
//
// # 为什么是三步而不是"成功了再扣"
//
// 不冻结就直接放行，用户可以在余额 10 分时并发提交 10 个各 10 分的任务，
// 每个都通过了余额检查，最后欠费 90 分。冻结把"检查"和"占用"变成一个原子动作，
// 并发提交的第二个请求会看到余额已经被第一个占掉了。
//
// # append-only 流水是唯一真相
//
// domain.LedgerEntry 表只追加、不更新、不删除。users.credits 是它的**物化缓存**，
// 存在的理由只是避免每次读余额都全表求和。两者对不上时，以流水重算的结果为准，
// 缓存改回来。
//
// 这条规则的价值在出问题的时候：金额对不上时可以逐条回放流水找出是哪一步错的。
// 如果只有一个可变的余额字段，出了问题连"什么时候开始错的"都查不出来。
//
// # 三类失败一律不扣费
//
// invalid_param / upstream_rate_limited / content_rejected —— PRD 定义的失败三分类，
// **都不扣费**。加上 insufficient_credit（压根没开始）与 internal_error（我们自己的锅），
// 实际上是"只要没拿到产物就不收钱"。
//
// domain.TaskError.Charged 因此恒为 false，但仍显式回传，让前端能明确显示
// 「未扣费」——用户失败时最关心的就是钱扣没扣，让他自己去流水里对是很差的体验。
//
// 取消同理：queued 取消全额退回；running 取消也不扣费。
package billing

import (
	"context"
	"errors"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// ErrAlreadySettled 是幂等闸挡下重复结算时挂在错误链上的哨兵。
//
// Charge / Refund 在任务已被 charge 或 refund 过时返回 domain.CodeConflict，
// 这是**正确行为**：重复的失败回调是常态（webhook 与轮询同时判失败、
// HTTP 取消与 executor 同时退款），挡不住就会把钱凭空变多。
//
// 但调用方光看 conflict 分不出"幂等闸挡的"与"真的没写进去"——外键指向的
// 行不存在、唯一键撞了同样归 409（见 store/mysql 的 wrap）。因此把这个
// 哨兵作为 cause 挂进去，让调用方用 errors.Is 精确判定，而不是靠错误码
// 或字符串猜。判错的代价是把真事故降级成 Info 静默掉。
//
// **它只标记"钱已经结清了"，不标记"这次调用做了什么"** ——
// 冻结额度在前一次结算时就已经释放，因此收到它时账面必然是平的。
var ErrAlreadySettled = errors.New("billing: task has already been settled")

// Ledger 是积分账本。
//
// 所有方法都必须在数据库事务里同时写入 domain.LedgerEntry 与更新 users.credits，
// 保证流水与缓存不会分叉。
//
// Hold 冻结额度。余额不足时返回 domain.CodeInsufficientCredit
// （HTTP 402），任务根本不会入队。
//
// Charge 结算一笔冻结额度。actual 通常等于冻结额，但允许小于（上游实际用量
// 比预估少时少收），**不允许大于**——用户看到的预估价就是他同意支付的上限，
// 超出部分只能平台自己承担。
//
// Refund 释放冻结额度，失败或取消时调用。
//
// Charge / Refund 撞上已结算的任务时返回 domain.CodeConflict，错误链上带
// ErrAlreadySettled；调用方要区分"幂等挡下的重复结算"与"真的没写进去"，
// 必须用 errors.Is 判这个哨兵，光看 Code 分不出来。
//
// Topup 是管理员手工充值（不接支付网关，PRD 定的）。amount 为正是充值、
// 为负是扣减。idempotencyKey 必填，防止管理员重复点击充两次；
// 同 key 二次调用返回 domain.CodeConflict。
//
// Balance 返回可用余额与冻结额度。
type Ledger interface {
	Hold(ctx context.Context, userID, taskID string, amount int) (domain.LedgerEntry, error)
	Charge(ctx context.Context, userID, taskID string, actual int) (domain.LedgerEntry, error)
	Refund(ctx context.Context, userID, taskID string, reason string) (domain.LedgerEntry, error)
	Topup(ctx context.Context, userID string, amount int, reason, operatorID, idempotencyKey string) (domain.LedgerEntry, error)
	Balance(ctx context.Context, userID string) (domain.Balance, error)
}
