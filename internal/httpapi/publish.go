package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/aigc-pool/aigc-pool/internal/asset"
	"github.com/aigc-pool/aigc-pool/internal/billing"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
)

func itoa(n int) string { return strconv.Itoa(n) }

// publish 发一批 SSE 事件。
//
// **发事件失败永远不影响本次请求的结果。** 事件是通知通道，不是事务的一部分；
// 让一次成功的提交因为推送失败而回 500，用户会重试，于是提交两次。
// 前端本来就有轮询兜底与 active 对账接口，漏一个事件不会丢状态。
func (s *server) publish(ctx context.Context, events ...stream.Event) {
	if s.deps.Stream == nil {
		return
	}
	for _, ev := range events {
		if err := s.deps.Stream.Publish(context.WithoutCancel(ctx), ev); err != nil {
			slog.Warn("推送事件失败", "type", ev.Type, "user_id", ev.UserID, "err", err)
		}
	}
}

// publishBalance 把最新余额推给用户。积分变动是用户最在意的那个数字，
// 让它等下一次页面刷新才更新，用户会以为扣错了。
func (s *server) publishBalance(ctx context.Context, userID string) {
	if s.deps.Stream == nil || s.deps.Ledger == nil {
		return
	}
	bal, err := s.deps.Ledger.Balance(ctx, userID)
	if err != nil {
		return
	}
	s.publish(ctx, stream.CreditUpdated(userID, bal))
}

// logRefund 记一次退款没能落库。
//
// 取消一条任务时**有两条路径都会调 Refund**（本接口与 executor 发现任务进
// 终态后退款），谁先到谁退成，晚到的那条必然撞上账本的幂等闸。那不是事故：
// 幂等闸返回 ErrAlreadySettled 恰恰证明冻结额度已经被前一次结算释放掉了，
// 钱一分没少。把它记成"需人工对账"会让看日志的人去追一笔根本不存在的欠账。
//
// 「需人工对账」四个字只留给真正的失败（库挂了、超时、约束撞了）——
// 那才是钱还冻着、只有人能收拾的场面。
func (s *server) logRefund(taskID string, err error) {
	if errors.Is(err, billing.ErrAlreadySettled) {
		slog.Info("退款已由另一条路径完成，跳过", "task_id", taskID)
		return
	}
	slog.Error("退款失败，冻结额度仍占用，需人工对账", "task_id", taskID, "err", err)
}

func (s *server) logCancelUpstream(taskID string, err error) {
	slog.Warn("通知上游取消失败，本地仍按取消处理", "task_id", taskID, "err", err)
}

// decorateAssets 把存储键翻译成可直接放进 <img src> 的内容 URL。
func (s *server) decorateAssets(items []domain.Asset) []domain.Asset {
	return asset.DecorateURLs(s.deps.Config.PublicBaseURL, items)
}
