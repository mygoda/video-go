// Package domain holds the platform's pure data types: tasks, assets, users,
// model configuration, canvas, credit ledger and skills.
//
// 本包是**零依赖**的。它不 import 本项目的任何其他包，也不 import 除标准库
// time 之外的任何东西。所有分层（L0 adapter / L1 capability / L2 executor /
// L3 asset / store / httpapi）都可以自由依赖 domain，而 domain 谁也不依赖——
// 这是保证依赖图无环的最简单办法。
//
// 字段形态以 docs/openapi.yaml 为准，openapi.yaml 又与
// docs/contracts/capability-schema.ts 逐字段对齐。三者任何一方单独改动都算破坏契约。
//
// 约定:
//   - 所有 ID 是 string，语义上不透明，不解析其内容。
//   - 所有时间是 time.Time，序列化为 RFC3339 UTC。
//   - openapi 里 `type: [x, 'null']` 的字段在这里是指针，用以区分「没有值」
//     和「值为零」——progress=0 与 progress=null 对前端是两件事。
//   - openapi 里的 JsonValue 在这里是 any。Go 没有 JSON 值的封闭类型，
//     any + encoding/json 是最贴近的表述。
package domain

import "time"

// TaskStatus 是**平台自己的**任务状态枚举。
//
// 各上游供应商的原始状态（ark 的 expired、sora 的 in_progress、
// google_lro 压根没有状态枚举只有 done 布尔）一律在 L0 driver 内部归一化到这五个值。
// L2 及以上只认这里的取值，绝不出现上游字符串。
type TaskStatus string

const (
	TaskStatusQueued    TaskStatus = "queued"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSucceeded TaskStatus = "succeeded"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCanceled  TaskStatus = "canceled"
)

// IsTerminal 报告该状态是否为终态。终态任务不再被 worker 领取，也不可再次取消。
func (s TaskStatus) IsTerminal() bool {
	return s == TaskStatusSucceeded || s == TaskStatusFailed || s == TaskStatusCanceled
}

// TaskErrorCode 与 capability-schema.ts 的 TaskErrorCode **逐值一致，不多不少**。
//
// 前三类是 PRD 定义的失败三分类。三类都不扣费（TaskError.Charged 恒为 false，
// 但仍显式回传，前端据此显示「未扣费」）。
//
// 传输层错误码（unauthorized / forbidden / not_found / conflict 等）**不在这里**，
// 它们属于 Error.Code 的取值域。分成两套是为了让本枚举能与 TypeScript 契约对拍：
// 任务失败分类是产品语义，401/403 是传输语义，混在一起会污染前端的失败三分类分支。
type TaskErrorCode string

const (
	TaskErrorInvalidParam        TaskErrorCode = "invalid_param"
	TaskErrorUpstreamRateLimited TaskErrorCode = "upstream_rate_limited"
	TaskErrorContentRejected     TaskErrorCode = "content_rejected"
	TaskErrorInsufficientCredit  TaskErrorCode = "insufficient_credit"
	TaskErrorInternal            TaskErrorCode = "internal_error"
)

// FieldError 定位到具体某个参数或输入槽。Key 对应 ParamSpec.Key 或 InputSlotSpec.Key，
// 前端据此回填并高亮出错的那枚芯片。
type FieldError struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// RetryInfo 仅在 upstream_rate_limited 时携带，用于前端展示「正在自动重试 (2/5)」。
type RetryInfo struct {
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	NextAt      *time.Time `json:"next_at,omitempty"`
}

// TaskError 是任务失败的结构化描述，挂在 Task.Error 与 SSE task.failed 事件上。
//
// Charged 显式回传而不是让前端推断：虽然失败三分类恒为 false，但「未扣费」这件事
// 必须由服务端断言，前端不做业务推理。
type TaskError struct {
	Code        TaskErrorCode `json:"code"`
	Message     string        `json:"message"`
	FieldErrors []FieldError  `json:"field_errors,omitempty"`
	Retryable   bool          `json:"retryable"`
	Charged     bool          `json:"charged"`
	Retry       *RetryInfo    `json:"retry,omitempty"`
}

// Task 是一次生成请求的完整记录。
//
// 提交请求里**没有 mode 字段**：传了 Inputs 就是图生图/图生视频，没传就是文生，
// 由服务端按输入槽是否为空做隐式分流。这个判断只在服务端发生。
type Task struct {
	ID        string     `json:"id"`
	UserID    string     `json:"-"`
	ModelID   string     `json:"model_id"`
	ModelName string     `json:"model_name,omitempty"`
	Modality  Modality   `json:"modality"`
	Status    TaskStatus `json:"status"`

	// Progress 上游大多不给真实百分比，前端靠 eta 做乐观推进（上限卡 90%）。
	Progress      *float64 `json:"progress"`
	QueuePosition *int     `json:"queue_position"`
	ETASeconds    *float64 `json:"eta_seconds"`

	Prompt string         `json:"prompt"`
	Params map[string]any `json:"params"`
	// Inputs 的 key 是 InputSlotSpec.Key，值是 upload_id 或已有 asset_id 的数组。
	//
	// 两种 id 混在同一个数组里，靠**解析顺序**区分：先按 asset_id 查，查不到
	// 再按 upload_id 查。提交侧（httpapi.assertInputRefs）与执行侧
	// （executor.resolveOne）必须用同一个顺序，否则同一个 id 在两处会解析成
	// 不同的东西。传 asset_id 是首选路径：它让「拿画布上已有的图当首帧」不必
	// 把字节取回来重传一遍，省掉一份存储、一次配额，血缘也直接指向原图。
	Inputs map[string][]string `json:"inputs,omitempty"`

	// EstimatedCost 是提交时冻结的额度；ActualCost 仅成功后有值，失败三分类恒为 0。
	EstimatedCost int  `json:"estimated_cost"`
	ActualCost    *int `json:"actual_cost"`

	Assets []Asset    `json:"assets,omitempty"`
	Error  *TaskError `json:"error,omitempty"`

	CanvasID *string `json:"canvas_id,omitempty"`
	// CardID 单卡重跑时携带：产物原地替换该卡片、旧版本进 history，
	// 位置/大小/refs 全部不变（前端设计的「锚点锁定」）。
	CardID *string `json:"card_id,omitempty"`

	// ClientToken 是幂等键。同一用户 24h 内重复提交同一个 token 直接返回已存在的任务，
	// 断网重发不会产生两个任务、不会重复扣费。
	ClientToken string `json:"client_token,omitempty"`

	// 以下是排障与执行用的内部字段，只在 admin 视图下发。
	ProviderID string `json:"provider_id,omitempty"`

	// Step 记录这一行是画布哪条同步链路落的（"script" / "storyboard" /
	// "refine"，历史行为 "legacy"）。空串表示走 executor 的普通任务。
	//
	// 它是**来源**，不是状态：那三条链路在 HTTP handler 里同步打完上游、把
	// 产物写进 canvas_cards 就地置终态，executor 从头到尾没碰过这一行。因此
	// 把它打回 queued 只会让 worker 花真钱重打一次上游，产物却落成一个孤儿
	// text 资产——画布上那张卡不会有任何变化。管理端的重试据此拒绝这些行。
	Step string `json:"step,omitempty"`

	Attempt           int     `json:"attempt,omitempty"`
	UpstreamRef       *string `json:"upstream_ref,omitempty"`
	UpstreamStatusRaw *string `json:"upstream_status_raw,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// TaskFilter 是任务列表查询条件，store.TaskRepo 与 admin 监控接口共用。
// status=active（queued + running）不分页，因为它是 SSE 重连后的对账接口，
// 分页会让对账本身失去意义。
type TaskFilter struct {
	UserID    string
	Statuses  []TaskStatus
	ModelID   string
	CanvasID  string
	ErrorCode TaskErrorCode
	Cursor    string
	Limit     int

	// IncludeDismissed 让被用户移除的任务重新进入结果集。
	// 默认 false（藏起来）是给用户侧的；管理端的任务监控显式置 true——
	// 用户把一张失败卡片收起来，不该让那条任务从故障排查的视野里消失。
	IncludeDismissed bool
}

// TaskModelStat 是某个模型在统计窗口内的表现。
type TaskModelStat struct {
	ModelID            string   `json:"model_id"`
	Total              int      `json:"total"`
	Failed             int      `json:"failed"`
	P50DurationSeconds *float64 `json:"p50_duration_seconds"`
}

// TaskStats 是 GET /api/admin/tasks/stats 的响应体。
//
// Queued / Running 单列而不是只给 ByStatus，是因为这两个数字回答的是运维当下
// 最想知道的那个问题（队列积压了吗、还有多少在跑），不该逼人从一个 map 里翻。
// OldestQueuedAgeSeconds 是积压是否恶化的唯一可靠信号：队列深度不变但最老的
// 那条越来越老，说明有任务卡住了而不是流量平稳。
type TaskStats struct {
	Window                 string                `json:"window"`
	Queued                 int                   `json:"queued"`
	Running                int                   `json:"running"`
	OldestQueuedAgeSeconds *float64              `json:"oldest_queued_age_seconds"`
	ByStatus               map[TaskStatus]int    `json:"by_status"`
	ByErrorCode            map[TaskErrorCode]int `json:"by_error_code"`
	ByModel                []TaskModelStat       `json:"by_model,omitempty"`
}
