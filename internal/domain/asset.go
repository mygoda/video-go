package domain

import "time"

// AssetType 是产物的媒体种类。
type AssetType string

const (
	AssetTypeImage AssetType = "image"
	AssetTypeVideo AssetType = "video"
	AssetTypeAudio AssetType = "audio"
	AssetTypeText  AssetType = "text"
)

// Asset 是一件已经落到**本平台自己存储**里的产物。
//
// 两条硬约束:
//  1. 必须带多档 URL（Original / Thumb512 / Poster）。画布同屏 20+ 张卡片，
//     不分级加载必炸。
//  2. 必须带 Source。没有它，「做同款」与「单卡重跑」同时废掉。
//
// Original / Thumb512 / Poster 均指向本平台的 /api/assets/{id}/content，
// **绝不是上游 URL**——上游 URL 24h 就失效，见 internal/asset.Transferor。
type Asset struct {
	ID     string    `json:"id"`
	UserID string    `json:"-"`
	Type   AssetType `json:"type"`

	Original string  `json:"original"`
	Thumb512 *string `json:"thumb_512"`
	Poster   *string `json:"poster"`

	// StorageKey 是本平台存储里的键，asset.Store 用它取二进制。
	// 只在服务端流转，不下发前端（前端只认 Original 这类 URL）。
	StorageKey string `json:"-"`

	MIME       string `json:"mime,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Width      *int   `json:"width,omitempty"`
	Height     *int   `json:"height,omitempty"`
	DurationMS *int   `json:"duration_ms,omitempty"`

	TaskID *string      `json:"task_id,omitempty"`
	Source *AssetSource `json:"source,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"-"`
}

// AssetSource 是生成参数快照——血缘的「怎么来的」那一半，
// 与 LineageRelation 的「从谁来的」互补。
//
// 「做同款」读的是这里：把 ModelID + Prompt + Params 原样回填进生成器。
type AssetSource struct {
	ModelID       string         `json:"model_id"`
	Prompt        string         `json:"prompt"`
	Params        map[string]any `json:"params"`
	InputAssetIDs []string       `json:"input_asset_ids"`
	SkillID       *string        `json:"skill_id,omitempty"`
}

// LineageRelation 是血缘图上一条边的语义。
//
//   - input:         to 是以 from 为输入生成的
//   - rerun_of:      to 是 from 的重跑版本（单卡重跑，锚点不变）
//   - composed_from: to 是由 from 等多个片段一键成片拼出来的
type LineageRelation string

const (
	LineageInput        LineageRelation = "input"
	LineageRerunOf      LineageRelation = "rerun_of"
	LineageComposedFrom LineageRelation = "composed_from"
)

// LineageEdge 是血缘图的一条有向边，From 是父资产，To 是子资产。
type LineageEdge struct {
	From     string          `json:"from"`
	To       string          `json:"to"`
	Relation LineageRelation `json:"relation"`
}

// LineageGraph 是 GET /api/assets/{id}/lineage 的响应体。
// 向上追输入（ancestors）、向下追派生（descendants），深度上限 5。
type LineageGraph struct {
	Nodes []Asset       `json:"nodes"`
	Edges []LineageEdge `json:"edges"`
}

// LineageDirection 是血缘查询方向。
type LineageDirection string

const (
	LineageAncestors   LineageDirection = "ancestors"
	LineageDescendants LineageDirection = "descendants"
	LineageBoth        LineageDirection = "both"
)

// Upload 是**临时对象**：24h 未被任务引用即回收；被任务引用后由 L3 提升为 Asset。
// 它与 Asset 分开，是因为「用户传上来的素材」和「平台生成的产物」在配额、
// 生命周期、血缘上的语义都不同。
type Upload struct {
	ID         string    `json:"upload_id"`
	UserID     string    `json:"-"`
	StorageKey string    `json:"-"`
	PreviewURL string    `json:"preview_url"`
	MIME       string    `json:"mime"`
	Bytes      int64     `json:"bytes"`
	Width      *int      `json:"width,omitempty"`
	Height     *int      `json:"height,omitempty"`
	Slot       string    `json:"-"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"-"`
	// AssetID 在该 upload 被任务引用并提升为 asset 后写入，回收扫描据此跳过它。
	AssetID *string `json:"-"`
}
