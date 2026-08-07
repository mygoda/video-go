package stream

import (
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// 以下五个结构体逐字段对应 openapi 的同名 schema。
//
// 它们放在本包而不是 domain，是因为它们是**事件负载**而不是领域实体：
// TaskUpdatedPayload 只挑了 domain.Task 里前端在进度更新时真正会读的几个字段，
// 把整条 Task 塞进每一次进度事件会让一条 12s 一次的 SSE 消息带上 prompt、
// params 和全部产物——一次生成期间要重复发十几遍。

// TaskUpdatedPayload 是 task.updated 的负载。
type TaskUpdatedPayload struct {
	TaskID        string            `json:"task_id"`
	Status        domain.TaskStatus `json:"status"`
	Progress      *float64          `json:"progress"`
	QueuePosition *int              `json:"queue_position"`
	ETASeconds    *float64          `json:"eta_seconds"`
}

// TaskSucceededPayload 是 task.succeeded 的负载。
//
// Assets 是完整的 domain.Asset：成功事件是前端把占位卡片换成真实产物的那一刻，
// 缺任何一档 URL 都会让卡片渲染不出来，因此这里不做裁剪。
type TaskSucceededPayload struct {
	TaskID     string         `json:"task_id"`
	Assets     []domain.Asset `json:"assets"`
	ActualCost int            `json:"actual_cost"`
}

// TaskFailedPayload 是 task.failed 的负载。
type TaskFailedPayload struct {
	TaskID string           `json:"task_id"`
	Error  domain.TaskError `json:"error"`
}

// CreditUpdatedPayload 是 credit.updated 的负载。
//
// 余额变动一律由服务端推，前端不做乐观扣减——冻结、结算、退款三步里
// 任何一步的金额与前端的猜测对不上，用户看到的就是余额自己跳了一下。
type CreditUpdatedPayload struct {
	Balance int `json:"balance"`
	Held    int `json:"held"`
}

// CanvasCardUpdatedPayload 是 canvas.card.updated 的负载。
type CanvasCardUpdatedPayload struct {
	CanvasID string            `json:"canvas_id"`
	CardID   string            `json:"card_id"`
	Status   domain.TaskStatus `json:"status"`
	AssetID  *string           `json:"asset_id"`
}

// TaskUpdated 构造一条任务进度事件。
func TaskUpdated(userID string, t domain.Task) Event {
	return Event{
		UserID: userID,
		Type:   EventTaskUpdated,
		Data: TaskUpdatedPayload{
			TaskID:        t.ID,
			Status:        t.Status,
			Progress:      t.Progress,
			QueuePosition: t.QueuePosition,
			ETASeconds:    t.ETASeconds,
		},
	}
}

// TaskSucceeded 构造一条任务成功事件。
func TaskSucceeded(userID, taskID string, assets []domain.Asset, actualCost int) Event {
	return Event{
		UserID: userID,
		Type:   EventTaskSucceeded,
		Data: TaskSucceededPayload{
			TaskID:     taskID,
			Assets:     assets,
			ActualCost: actualCost,
		},
	}
}

// TaskFailed 构造一条任务失败事件。
func TaskFailed(userID, taskID string, terr domain.TaskError) Event {
	return Event{
		UserID: userID,
		Type:   EventTaskFailed,
		Data:   TaskFailedPayload{TaskID: taskID, Error: terr},
	}
}

// CreditUpdated 构造一条余额变动事件。
func CreditUpdated(userID string, b domain.Balance) Event {
	return Event{
		UserID: userID,
		Type:   EventCreditUpdated,
		Data:   CreditUpdatedPayload{Balance: b.Available, Held: b.Held},
	}
}

// CanvasCardUpdated 构造一条画布卡片更新事件。
func CanvasCardUpdated(userID, canvasID, cardID string, status domain.TaskStatus, assetID *string) Event {
	return Event{
		UserID: userID,
		Type:   EventCanvasCardUpdated,
		Data: CanvasCardUpdatedPayload{
			CanvasID: canvasID,
			CardID:   cardID,
			Status:   status,
			AssetID:  assetID,
		},
	}
}
