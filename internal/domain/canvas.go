package domain

import "time"

// Project 是一个画布项目。
//
// Revision 是画布的乐观锁：客户端提交增量 op 时带上 base_revision，
// 不匹配就是另一个标签页动过同一画布，返回 409 让前端拉全量覆盖本地。
// MVP 明确不做 CRDT（不做协作）。
type Project struct {
	ID           string    `json:"id"`
	UserID       string    `json:"-"`
	Name         string    `json:"name"`
	Revision     int64     `json:"revision"`
	CardCount    int       `json:"card_count,omitempty"`
	CoverAssetID *string   `json:"cover_asset_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CardKind 是卡片承载的内容类型。
type CardKind string

const (
	CardKindText  CardKind = "text"
	CardKindImage CardKind = "image"
	CardKindVideo CardKind = "video"
)

// CardVersion 是卡片的一个历史版本，重跑时旧版本入 history，
// 供卡片左下角的版本切换器 ‹2/3› 使用。
type CardVersion struct {
	AssetID string    `json:"asset_id"`
	Prompt  string    `json:"prompt"`
	At      time.Time `json:"at"`
}

// Card 是画布上的一张卡片。
//
// Refs 是**血缘**：这张卡由哪些卡片作为输入生成。它不是可视连线——
// 「多卡引用为下次输入」（AutoLink 的本质）靠的就是它，不需要用户拉线。
//
// AutoPlaced 在用户手动移动过之后置 false，该卡片所在区域不再参与自动重排。
type Card struct {
	ID   string   `json:"id"`
	Kind CardKind `json:"kind"`
	// Title 与 Refs / History 同理恒定序列化，不加 omitempty：
	// 前端六处直接读它，少一个字段就是一次白屏。空串是合法值
	// （本字段是后加的，历史卡片没有标题）。
	Title string  `json:"title"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Z     float64 `json:"z"`

	TaskID  *string `json:"task_id"`
	AssetID *string `json:"asset_id"`
	Text    *string `json:"text"`
	ModelID *string `json:"model_id"`
	Prompt  *string `json:"prompt"`

	Params map[string]any `json:"params,omitempty"`
	// Refs / History 恒定序列化为数组，绝不缺失也绝不为 null：
	// 前端直接读 .length，少一个字段就是一次白屏。
	Refs    []string      `json:"refs"`
	History []CardVersion `json:"history"`

	AutoPlaced bool      `json:"auto_placed"`
	CreatedAt  time.Time `json:"created_at"`
}

// MessageRole 是画布对话中一条消息的角色。
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleSystem    MessageRole = "system"
)

// Message 是画布内对话的一条消息。
//
// RefCardIDs 是「多卡引用为下次输入」的载体。TaskIDs 记录 agent 在这条回复里
// 直接产出的生成任务，其后续状态走 SSE。
type Message struct {
	ID         string      `json:"id"`
	ProjectID  string      `json:"-"`
	Role       MessageRole `json:"role"`
	Content    string      `json:"content"`
	SkillID    *string     `json:"skill_id"`
	RefCardIDs []string    `json:"ref_card_ids,omitempty"`
	TaskIDs    []string    `json:"task_ids,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
}

// Viewport 是上次退出时的视口，k ∈ [0.1, 4]。
type Viewport struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	K float64 `json:"k"`
}

// CanvasSnapshot 是画布全量，对应 GET /api/projects/{id}/canvas。
type CanvasSnapshot struct {
	Revision     int64     `json:"revision"`
	Cards        []Card    `json:"cards"`
	Conversation []Message `json:"conversation"`
	Viewport     *Viewport `json:"viewport,omitempty"`
}

// CanvasOpType 是增量 op 的类型。
type CanvasOpType string

const (
	OpCardCreate  CanvasOpType = "card.create"
	OpCardMove    CanvasOpType = "card.move"
	OpCardUpdate  CanvasOpType = "card.update"
	OpCardDelete  CanvasOpType = "card.delete"
	OpViewportSet CanvasOpType = "viewport.set"
)

// CanvasOp 是一条增量操作。
//
// 与 ParamSpec 同理，Go 没有和 TypeScript 判别联合等价的类型，因此这里也用
// 「一个结构体 + Type 判别字段 + 各分支字段可选」的形态：Card 只在 card.create
// 时有值，X/Y/Z 只在 card.move 时有值，以此类推。判别逻辑集中在 canvas.Applier。
//
// 不做全量 PUT 而做增量 op，一是几百张卡片每次几百 KB 纯浪费，
// 二是服务端按顺序落库这串 op 天然满足「创作过程可回放」。
type CanvasOp struct {
	Type CanvasOpType `json:"type"`

	// card.create
	Card *Card `json:"card,omitempty"`

	// card.move / card.update / card.delete
	ID    string         `json:"id,omitempty"`
	X     *float64       `json:"x,omitempty"`
	Y     *float64       `json:"y,omitempty"`
	Z     *float64       `json:"z,omitempty"`
	Patch map[string]any `json:"patch,omitempty"`

	// viewport.set
	Viewport *Viewport `json:"viewport,omitempty"`
}
