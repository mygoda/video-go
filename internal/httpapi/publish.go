package httpapi

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/aigc-pool/aigc-pool/internal/asset"
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

func (s *server) logRefund(taskID string, err error) {
	slog.Error("退款失败，冻结额度仍占用，需人工对账", "task_id", taskID, "err", err)
}

func (s *server) logCancelUpstream(taskID string, err error) {
	slog.Warn("通知上游取消失败，本地仍按取消处理", "task_id", taskID, "err", err)
}

// decorateAssets 把存储键翻译成可直接放进 <img src> 的内容 URL。
//
// 存储键（user/xx/asset/yy.png）是内部布局，一旦下发给前端就等于把它焊死：
// 换一个存储后端所有历史资产的地址全变。内容 URL 只暴露 asset id，
// 布局怎么改都不影响前端。
func (s *server) decorateAssets(items []domain.Asset) []domain.Asset {
	base := s.deps.Config.PublicBaseURL
	out := make([]domain.Asset, 0, len(items))
	for _, a := range items {
		a.Original = asset.ContentURL(base, a.ID, asset.VariantOriginal)
		if a.ThumbKey != nil {
			u := asset.ContentURL(base, a.ID, asset.VariantThumb512)
			a.Thumb512 = &u
		} else {
			a.Thumb512 = nil
		}
		if a.PosterKey != nil {
			u := asset.ContentURL(base, a.ID, asset.VariantPoster)
			a.Poster = &u
		} else {
			a.Poster = nil
		}
		out = append(out, a)
	}
	return out
}
