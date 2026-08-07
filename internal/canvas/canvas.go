// Package canvas applies incremental operation lists to a persisted canvas.
//
// # 为什么是增量 op 而不是全量 PUT
//
// 一个画布几百张卡片，全量提交每次几百 KB。用户拖一下卡片就发一次全量，
// 纯浪费带宽，而且并发写的最后一次会静默覆盖前面所有人的修改。
//
// 增量 op 还有个副产品：服务端按顺序落库这串 op，**「创作过程可回放」天然成立**，
// 不需要为此单独设计一套录制机制。
//
// # 冲突处理：乐观锁，不做 CRDT
//
// 客户端提交时带 base_revision。不匹配说明另一个标签页动过同一画布，
// 直接返回 domain.CodeRevisionConflict（409），前端拉全量覆盖本地。
//
// MVP **明确不做 CRDT**——产品上不做多人协作，唯一的冲突场景是同一个人开了
// 两个标签页，频率极低且用户自己就能理解"另一个窗口改过了"。
// 为这个场景引入 CRDT 的复杂度完全不划算。
package canvas

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// Applier 把一串有序 op 应用到某个画布上。
//
// Apply 在一个事务里:
//  1. 校验 baseRevision 与画布当前 revision 相等，不等则返回 CodeRevisionConflict；
//  2. **按数组顺序**依次应用 ops（顺序有意义：先 create 后 move 与反过来不同）；
//  3. revision 加一并返回。
//
// 整串 op 要么全成功要么全不生效：半截应用会让客户端的本地状态与服务端
// 对不上，而客户端此时并不知道自己该回滚到哪。
//
// Snapshot 返回画布全量，冲突后前端用它覆盖本地。
type Applier interface {
	Apply(ctx context.Context, projectID string, baseRevision int64, ops []domain.CanvasOp) (int64, error)
	Snapshot(ctx context.Context, projectID string) (domain.CanvasSnapshot, error)
}
