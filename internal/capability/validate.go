package capability

import "github.com/aigc-pool/aigc-pool/internal/domain"

// Submission 是一次待校验的任务提交，字段与 POST /api/tasks 的请求体对应。
//
// 注意**没有 mode 字段**：传了 Inputs 就是图生图/图生视频，没传就是文生，
// 隐式分流由服务端按输入槽是否为空判定，前端不参与。
type Submission struct {
	ModelID string
	Prompt  string
	Inputs  map[string][]string
	Params  map[string]any
}

// Validator 对一次提交做服务端校验。
//
// 前端已经按 schema 渲染并预校验过一遍，但那只是体验优化——**服务端必须独立再校验一遍**，
// 因为请求可以绕过前端直接构造。这里是唯一可信的那一遍。
//
// 校验范围:
//   - 未知的 params key / inputs slot 一律拒绝（防止污染上游请求体）；
//   - 每个参数的取值落在其 ParamSpec 声明的域内（select 在 options 里、
//     stepper/slider 在 [Min,Max] 且对齐 Step、textarea 不超 MaxLength）；
//   - VisibleWhen 为假的参数**不允许出现**在 Params 里（前端约定不提交它们，
//     出现即代表来路可疑或前端有 bug）；
//   - 必填输入槽有值，可选槽的数量落在 [MinCount, MaxCount]，MIME 在 Accept 白名单内；
//   - prompt 长度不超过 LimitSpec.PromptMaxLength；
//   - ParamSpec 结构自洽（见 ValidateSchema）。
//
// Validate 失败时返回的 FieldError 列表直接作为 TaskError.FieldErrors 下发，
// Key 必须是 ParamSpec.Key 或 InputSlotSpec.Key——前端靠它高亮出错的那枚芯片。
// 因此实现应**收集全部错误再返回**，而不是遇到第一个就短路：一次提交里三个参数
// 都填错，用户应当一次看到三个红点，而不是改一个试一次。
//
// ValidateSchema 校验 ModelCapabilitySchema 自身是否自洽，用于管理后台
// POST/PATCH /api/admin/models 落库前把关。包注释里说过，ParamSpec 是所有控件
// 字段的并集结构，类型系统不阻止给 toggle 填 Options——那条约束就在这里兑现：
// 每种 Control 必须携带它要求的字段（select 要 Options、stepper 要 Min/Max/Step、
// compound 要 Fields + DisplayTemplate 且自身 Default 为 nil），
// 且不得携带其他控件专属的字段。写错配置应在保存时就被拒，
// 而不是等某个用户提交任务时才炸。
type Validator interface {
	Validate(schema ModelCapabilitySchema, sub Submission) []domain.FieldError
	ValidateSchema(schema ModelCapabilitySchema) []domain.FieldError
}
