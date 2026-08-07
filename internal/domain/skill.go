package domain

// Skill 是一条**可插拔的记录**（预置系统提示词 + 默认模型参数），不是硬编码分支。
//
// 新增一个玩法 = 加一条 Skill 记录，代码零改动。这与「接新模型 = 加一条
// ModelConfig」是同一条设计线。
//
// SystemPrompt 原文**不下发前端**：用户端只拿到 SystemPromptRef 这个标识，
// 管理端（AdminSkill）才看得到原文。序列化时由 httpapi 层按路由组选择投影，
// 因此这里 SystemPrompt 标 json:"-"，避免误从用户端漏出。
type Skill struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Description     string         `json:"description,omitempty"`
	SystemPromptRef string         `json:"system_prompt_ref,omitempty"`
	SystemPrompt    string         `json:"-"`
	DefaultModelID  *string        `json:"default_model_id"`
	DefaultParams   map[string]any `json:"default_params"`
	Enabled         bool           `json:"-"`
	Order           int            `json:"order"`
}
