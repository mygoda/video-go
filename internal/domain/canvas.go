package domain

import (
	"encoding/json"
	"sort"
	"time"
)

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
	// CardKindScript 是剧本卡，正文放在 Text，历史版本放在 Params
	// （见 ScriptParams）。在它存在之前剧本只是 canvas_messages 里的一条
	// 聊天记录——看得见但改不动，更重来不了。
	CardKindScript CardKind = "script"
	// CardKindShot 是镜头卡，结构化字段放在 Params（见 ShotParams），
	// Refs 指回它所属的剧本卡。它是「花钱出片之前能改」的那个对象。
	CardKindShot CardKind = "shot"
)

// KnownCardKinds 是全部合法的卡片类型，落库白名单以它为准。
var KnownCardKinds = []CardKind{
	CardKindText, CardKindImage, CardKindVideo, CardKindScript, CardKindShot,
}

// Valid 报告该 kind 是否为已知类型。
func (k CardKind) Valid() bool {
	for _, known := range KnownCardKinds {
		if k == known {
			return true
		}
	}
	return false
}

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

// ShotParams 是 shot 卡放在 Card.Params 里的结构化字段。
//
// # 为什么台词必须是独立字段，而不是揉进 Description
//
// 上游 Doubao-Seedance-2.0-fast 是音画同步生成：台词写进 prompt，出片就带同步
// 的中文人声（实测 prompt 含「你终于来了」，成片音轨 whisper 转写一字不差；
// 不含台词时音轨只有环境音）。也就是说人声不需要 TTS、不需要新模型、不额外
// 花钱，它是 prompt 层的事——**前提是拼 prompt 的时候能把台词单独拎出来**。
// 字幕同理：台词本来就是用户自己写的，直接拿这个字段生成，零成本零误差，
// 不需要对成片做语音转写。这两件事只要台词一旦被并进镜头描述就都做不成了。
//
// # 为什么不新建 shots 表
//
// 见 000014 迁移的注释：canvas_cards 已经有增删改查、软删、revision 推进和
// 回放，再造一张表换不来新能力，只换来两份必须同步演进的代码。
//
// 所有字段恒定序列化（不加 omitempty），理由同 Card.Title：前端直接读，
// 少一个字段就是一次白屏；空串 / 0 是合法值（这一镜没写台词、时长还没定、
// 首帧还没出）。
type ShotParams struct {
	// ShotNo 是镜号，从 1 开始。它是镜头在剧本内的顺序，也是拼片的依据。
	ShotNo int `json:"shot_no"`
	// Description 是镜头描述（画面内容），出片时进 prompt。**不含台词。**
	Description string `json:"description"`
	// Dialogue 是这一镜的台词，可以为空（空镜、纯环境音）。
	Dialogue string `json:"dialogue"`
	// DurationSec 是镜头时长（秒）。0 表示还没定，由出片那一步取模型默认值。
	DurationSec float64 `json:"duration_sec"`
	// Camera 是机位 / 运镜，如「手持跟拍」「固定」。自由文本：上游吃的是
	// 自然语言 prompt，收敛成枚举只会把模型认得的说法挡在外面。
	Camera string `json:"camera"`
	// ShotSize 是景别，如「特写」「中景」「全景」。自由文本，理由同 Camera。
	ShotSize string `json:"shot_size"`
	// FirstFrameAssetID 是这一镜的首帧图，空串表示还没出。
	//
	// 它不能挂在 Card.AssetID 上：出片那一步要把视频往同一个字段写，首帧当场
	// 就没了，Card.History 也会把图和视频混成一串。首帧是镜头的一个**属性**
	// （「这一镜长什么样」），不是画布上另一个可以单独摆位、单独引用的对象，
	// 所以它跟镜号、台词一样住在 params 里。
	FirstFrameAssetID string `json:"first_frame_asset_id"`
}

// shotParamKeys 是 ShotParams 全部合法的 JSON key，用于拒绝未知字段。
var shotParamKeys = map[string]struct{}{
	"shot_no": {}, "description": {}, "dialogue": {},
	"duration_sec": {}, "camera": {}, "shot_size": {},
	"first_frame_asset_id": {},
}

// ParseShotParams 把 shot 卡的 params 解成 ShotParams 并校验。
//
// # 为什么未知 key 报错而不是留着
//
// 这个 schema 存在的全部意义就是台词有它自己的位置。而把台词写进 `dialog`、
// `lines`、`speech` 这类近义 key，在一个「宽松接收」的实现里会安静地存下来，
// 出片时拼 prompt 的代码读 `dialogue` 读到空串，成片就是没有人声——和这张票
// 要修的原始故障一模一样，且没有任何报错可查。宁可在写入那一刻就顶回去。
//
// 空 params 直接放行：镜头卡可以先建出来再填（分镜那一步会分批回写）。
func ParseShotParams(params map[string]any) (ShotParams, error) {
	var sp ShotParams
	if len(params) == 0 {
		return sp, nil
	}

	var fields []FieldError
	for key := range params {
		if _, ok := shotParamKeys[key]; !ok {
			fields = append(fields, FieldError{
				Key:     "params." + key,
				Message: "shot 卡不认识这个字段，台词请用 dialogue，画面描述请用 description",
			})
		}
	}
	if len(fields) > 0 {
		sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		return sp, shotParamsError(fields)
	}

	// 先 Marshal 再 Unmarshal：params 是 map[string]any（JSON 解出来的形态），
	// 逐 key 做类型断言要写六遍近乎一样的分支，而 encoding/json 的类型错误
	// 本来就带字段名。
	raw, err := json.Marshal(params)
	if err != nil {
		return sp, shotParamsError([]FieldError{{Key: "params", Message: err.Error()}})
	}
	if err := json.Unmarshal(raw, &sp); err != nil {
		return sp, shotParamsError([]FieldError{{Key: "params", Message: err.Error()}})
	}

	if sp.ShotNo < 1 {
		fields = append(fields, FieldError{
			Key: "params.shot_no", Message: "镜号从 1 开始",
		})
	}
	if sp.DurationSec < 0 {
		fields = append(fields, FieldError{
			Key: "params.duration_sec", Message: "时长不能为负",
		})
	}
	if len(fields) > 0 {
		return sp, shotParamsError(fields)
	}
	return sp, nil
}

func shotParamsError(fields []FieldError) error {
	return &Error{
		Code:        CodeInvalidParam,
		Message:     "shot 卡的 params 不合法",
		FieldErrors: fields,
	}
}

// ScriptMaxVersions 是一张剧本卡最多留多少个历史版本，超出后丢最旧的。
//
// 版本存在 params 这一个 JSON 列里（票面要求：别为此建表），而一份剧本正文
// 是几 KB 量级——不封顶的话，一个改到第两千版的剧本会把单行撑到十兆，
// 而拉一次画布全量要把这一行整个读出来，画布上每张卡都会跟着变慢。
// 50 版覆盖得住"一下午来回改"的量级，再往前的版本对用户已经没有意义。
const ScriptMaxVersions = 50

// ScriptParams 是 script 卡放在 Card.Params 里的结构化字段。
//
// 当前正文始终在 Card.Text，params 里**只放历史版本**——两者不重复存。
// 反过来（params 里存全部版本、Text 只是缓存）会立刻多出一个"谁是权威"的
// 问题，而画布上所有既有读路径读的都是 Text。
type ScriptParams struct {
	// Versions 是这张卡每次保存前的旧正文，按 Rev 升序，最后一项是上一版。
	// 恒定序列化不加 omitempty，理由同 Card.Title：前端直接读 .length。
	Versions []ScriptVersion `json:"versions"`
}

// ScriptVersion 是剧本卡的一个历史版本，即某次保存**之前**的内容。
//
// 不复用 Card.History：那个结构是 {asset_id, prompt, at}，为"重跑换了一版
// 产物"设计的，三个字段对剧本一个都用不上（剧本没有 asset、没有 prompt）。
type ScriptVersion struct {
	// Rev 从 1 开始，1 是这张卡最初的正文。它是全局递增的编号，不是数组下标——
	// 超过 ScriptMaxVersions 丢掉最旧的之后两者就不再相等，而前端要显示的
	// 「第几版」必须还是原来那个数。当前正文的版号是最后一版加一。
	Rev int `json:"rev"`
	// Title / Text 是这一版的标题与正文。标题一起留：回退时只还原正文会让
	// 卡片顶着一个属于新版的标题，看上去像回退失败了。
	Title string `json:"title"`
	Text  string `json:"text"`
	// At 是这一版被替换掉的时刻，也就是那次保存发生的时间。
	At time.Time `json:"at"`
}

// scriptParamKeys 是 ScriptParams 全部合法的 JSON key。
var scriptParamKeys = map[string]struct{}{"versions": {}}

// ParseScriptParams 把 script 卡的 params 解成 ScriptParams 并校验。
//
// 未知 key 报错的理由同 ParseShotParams：这个 schema 存在的意义就是版本
// 有它自己的位置，把版本写进 `history`、`revisions` 这类近义 key 会被安静
// 存下来，而前端读 `versions` 读到空数组——用户看到的就是"改了半天一版
// 没留下"，且没有任何报错可查。
func ParseScriptParams(params map[string]any) (ScriptParams, error) {
	var sp ScriptParams
	sp.Versions = []ScriptVersion{}
	if len(params) == 0 {
		return sp, nil
	}

	var fields []FieldError
	for key := range params {
		if _, ok := scriptParamKeys[key]; !ok {
			fields = append(fields, FieldError{
				Key:     "params." + key,
				Message: "script 卡只认识 versions 一个字段，正文请放在 text",
			})
		}
	}
	if len(fields) > 0 {
		sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		return sp, scriptParamsError(fields)
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return sp, scriptParamsError([]FieldError{{Key: "params", Message: err.Error()}})
	}
	if err := json.Unmarshal(raw, &sp); err != nil {
		return sp, scriptParamsError([]FieldError{{Key: "params", Message: err.Error()}})
	}
	if sp.Versions == nil {
		sp.Versions = []ScriptVersion{}
	}
	return sp, nil
}

// AppendVersion 把一份旧正文追加成新的一版，并按 ScriptMaxVersions 截断。
//
// 编号取"最后一版 + 1"而不是 len+1：截断丢掉最旧几版之后 len 会倒退，
// 用它编号会让新版本拿到一个已经用过的号，前端的版本列表就会出现两个 v37。
func (sp ScriptParams) AppendVersion(title, text string, at time.Time) ScriptParams {
	next := 1
	if n := len(sp.Versions); n > 0 {
		next = sp.Versions[n-1].Rev + 1
	}
	versions := append(sp.Versions, ScriptVersion{
		Rev: next, Title: title, Text: text, At: at.UTC(),
	})
	if len(versions) > ScriptMaxVersions {
		versions = versions[len(versions)-ScriptMaxVersions:]
	}
	return ScriptParams{Versions: versions}
}

func scriptParamsError(fields []FieldError) error {
	return &Error{
		Code:        CodeInvalidParam,
		Message:     "script 卡的 params 不合法",
		FieldErrors: fields,
	}
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
