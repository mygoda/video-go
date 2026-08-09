/**
 * Capability Schema —— 前端动态表单的唯一契约。
 *
 * 本文件是 docs/contracts/capability-schema.ts 的逐字副本（frontend-design.md §9）。
 * 契约变更走 docs/，改完同步回这里，不在这里单方面加字段。
 *
 * 这份文件是 DEM-61 前端设计的产出物，同时是 L1「能力声明层」对后端的输入约束：
 * 后端 `GET /api/models` 必须返回符合 ModelCapabilitySchema 的对象数组。
 *
 * 验收线：接入一个新模型 = 新增一份 ModelCapabilitySchema + 一个 L0 适配器，前端零改动。
 * 只有当新模型需要一种全新的 ControlKind 时，才允许改前端。
 */

export type JsonPrimitive = string | number | boolean | null;
export type JsonValue = JsonPrimitive | JsonValue[] | { [k: string]: JsonValue };

/* ────────────────────────────── 顶层 ────────────────────────────── */

export interface ModelCapabilitySchema {
  /** 稳定标识，提交任务时原样回传。前端只做透传，绝不解析其内容 */
  id: string;
  /** 展示名，后端给什么显示什么 */
  name: string;
  /** 供应商，仅用于展示与故障提示（"该供应商暂时不可用"）。前端禁止按此分支 */
  vendor: string;
  /**
   * 决定归入哪个 tab。前端 tab 结构固定为 image / video 两个。
   * text 没有对应的 tab：chat 族模型是分镜拆解这类平台内部能力的载体，
   * 不是用户能在创作页选的产品，用户端目录里也不会出现（后端按 visibility 摘掉）。
   * 它出现在这个联合类型里，只为管理端能如实显示与编辑。
   */
  modality: 'image' | 'video' | 'text';
  /** false 时在选择器中置灰并显示 disabled_reason */
  enabled: boolean;
  disabled_reason?: string;
  /** 同 modality 下的排序权重，小的在前；相同则按 name */
  order: number;
  /** 模型简介，展示在选择器的二级说明行 */
  description?: string;
  /** 缩略示例图 URL，模型选择器用 */
  preview_url?: string;

  inputs: InputSlotSpec[];
  params: ParamSpec[];
  pricing: PricingSpec;
  eta: EtaSpec;
  limits: LimitSpec;
}

/* ───────────────────────── 输入槽（隐式分流的载体） ───────────────────────── */

/**
 * 输入槽决定「传了图走图生图 / 没传走文生」这条隐式分流。
 * 前端不发送 mode 字段，只发送 prompt + inputs；由后端根据 inputs 是否为空路由。
 * required=false 的槽 = 可选输入 = 隐式分流点。
 */
export interface InputSlotSpec {
  /** 提交时的 key，如 "reference_images" / "first_frame" / "last_frame" */
  key: string;
  /** 上传位上的文案，如 "参考图" / "首帧" */
  label: string;
  kind: 'image' | 'video' | 'audio';
  /** false = 可选（隐式分流）；true = 该模型必须传（如纯图生视频模型） */
  required: boolean;
  /** 允许的数量区间。max=1 时渲染为单图位，>1 渲染为可增删的图组 */
  min_count: number;
  max_count: number;
  /** MIME 白名单，直接喂给 <input accept> 并在前端预校验 */
  accept: string[];
  max_bytes: number;
  /** 尺寸约束，前端读取图片后即时校验，避免上传完才报错 */
  min_pixels?: { width: number; height: number };
  max_pixels?: { width: number; height: number };
  /** 悬浮提示 */
  hint?: string;
}

/* ───────────────────────────── 参数与控件 ───────────────────────────── */

/**
 * 控件枚举。这 8 种覆盖了 liblib.art 实测的全部参数芯片。
 * 新增枚举值 = 需要一次前端改动，因此新增前先确认现有控件真的表达不了。
 * 前端遇到未知 control 必须降级渲染（见 §控件映射表），不允许白屏。
 */
export type ControlKind =
  | 'select'         // 单选下拉芯片：分辨率 / 时长 / 生成模式 / 风格模型
  | 'aspect_select'  // 比例选择，带可视化缩略比例框
  | 'compound'       // 一个芯片承载多个字段，如「1:1 · 1张」
  | 'stepper'        // ⊖ n ⊕，如生成数量
  | 'slider'         // 滑杆 + 数字输入，如重绘强度
  | 'toggle'         // 开关，如「提示词自动翻译」
  | 'seed'           // 数字输入 + 随机 + 锁定
  | 'textarea';      // 多行文本，如负向提示词

export interface ParamSpecBase {
  /** 提交时 params 对象的 key */
  key: string;
  label: string;
  control: ControlKind;
  /** primary → 渲染进主参数芯片条；advanced → 收进 ⚙ 折叠面板 */
  group: 'primary' | 'advanced';
  /** 同 group 内的排列顺序 */
  order: number;
  /** 初始值。切换模型时若旧值在新 schema 下非法，回落到这里 */
  default: JsonValue;
  help?: string;
  /** 不满足时该参数整体不渲染，且提交时不带该 key */
  visible_when?: Condition;
  /** 不满足时渲染为禁用态，附 disabled_hint */
  enabled_when?: Condition;
  disabled_hint?: string;
}

export interface OptionItem {
  value: JsonPrimitive;
  label: string;
  /** 选项右侧小标签，如 "+2 积分" / "慢" */
  badge?: string;
  disabled?: boolean;
  disabled_reason?: string;
}

export interface SelectSpec extends ParamSpecBase {
  control: 'select';
  options: OptionItem[];
  /** 选项 ≤ 4 时渲染成一排分段按钮，否则渲染成下拉。后端可强制指定 */
  render_hint?: 'dropdown' | 'segmented';
}

export interface AspectOption extends OptionItem {
  /** 用于画出可视比例框 */
  ratio_w: number;
  ratio_h: number;
  /** 该比例实际出图像素，展示在选项副标题 */
  pixels?: { width: number; height: number };
}

export interface AspectSelectSpec extends ParamSpecBase {
  control: 'aspect_select';
  options: AspectOption[];
}

export interface StepperSpec extends ParamSpecBase {
  control: 'stepper';
  min: number;
  max: number;
  step: number;
  unit?: string; // "张"
}

export interface SliderSpec extends ParamSpecBase {
  control: 'slider';
  min: number;
  max: number;
  step: number;
  unit?: string;
}

export interface ToggleSpec extends ParamSpecBase {
  control: 'toggle';
}

export interface SeedSpec extends ParamSpecBase {
  control: 'seed';
  min: number;
  max: number;
  /** true 时芯片上出现 🎲；default 为 null 表示每次随机 */
  allow_random: boolean;
}

export interface TextareaSpec extends ParamSpecBase {
  control: 'textarea';
  max_length: number;
  placeholder?: string;
}

/**
 * 复合控件：把多个语义相关的字段塞进同一个芯片，减少交互点。
 * liblib 的「1:1 · 1张」就是 aspect + count 的 compound。
 * 提交时 fields 各自作为独立 key 平铺进 params，compound 本身不产生 key。
 */
export interface CompoundSpec extends ParamSpecBase {
  control: 'compound';
  fields: ParamSpec[];
  /** 芯片标签模板，用 {fieldKey} 占位，如 "{aspect} · {count}张" */
  display_template: string;
  /** compound 自身 default 恒为 null，真实默认值在各 field 内 */
  default: null;
}

export type ParamSpec =
  | SelectSpec
  | AspectSelectSpec
  | CompoundSpec
  | StepperSpec
  | SliderSpec
  | ToggleSpec
  | SeedSpec
  | TextareaSpec;

/* ─────────────────────────────── 条件表达式 ─────────────────────────────── */

/**
 * 参数联动与「隐式分流」的声明式表达。
 * 例：重绘强度只在传了参考图时出现 → visible_when: { op:'has_input', slot:'reference_images' }
 * 前端实现是一个纯函数 evaluate(cond, formValues, inputs): boolean，约 40 行，无需为新模型改动。
 */
export type Condition =
  | { op: 'eq'; key: string; value: JsonPrimitive }
  | { op: 'ne'; key: string; value: JsonPrimitive }
  | { op: 'gt'; key: string; value: number }
  | { op: 'lt'; key: string; value: number }
  | { op: 'in'; key: string; value: JsonPrimitive[] }
  | { op: 'nin'; key: string; value: JsonPrimitive[] }
  /** 该输入槽当前有文件 */
  | { op: 'has_input'; slot: string }
  | { op: 'and'; of: Condition[] }
  | { op: 'or'; of: Condition[] }
  | { op: 'not'; of: Condition };

/* ─────────────────────────────── 计费与耗时 ─────────────────────────────── */

/**
 * 前端必须能在用户改参数时**本地实时**算出预估积分，否则每动一下芯片就要打一次后端。
 * 算法（前端实现，约 30 行）：
 *   subtotal = base
 *   for m of modifiers: if match(m.when) then subtotal = m.op==='mul' ? subtotal*m.value : subtotal+m.value
 *   if multiplier_param: subtotal *= Number(params[multiplier_param])
 *   cost = round(subtotal, rounding)
 * 前端算的只用于展示。以 POST /api/tasks 返回的 estimated_cost 为准，不一致时以后端覆盖显示。
 */
export interface PricingSpec {
  currency: 'credit';
  base: number;
  /** 按数组顺序依次应用 */
  modifiers: PricingModifier[];
  /** 最后乘上该参数的数值，如 count（几张）或 duration（几秒） */
  multiplier_param?: string;
  rounding: 'ceil' | 'round' | 'floor';
}

export interface PricingModifier {
  when: Condition;
  op: 'mul' | 'add';
  value: number;
  /** 展示用，如 "1080P 加价"。悬浮在费用估算上时列出命中的规则 */
  label?: string;
}

/**
 * 供应商大多不给真实百分比，前端靠 eta 做乐观进度推进（上限卡 90%）。
 * scales_with 指向某个数值参数时，eta 按 该参数值 / 该参数默认值 线性缩放。
 */
export interface EtaSpec {
  p50_seconds: number;
  p90_seconds: number;
  scales_with?: string;
}

export interface LimitSpec {
  /** 前端据此在达到上限时禁用提交按钮并提示，而不是让用户提交后被拒 */
  max_concurrent_per_user: number;
  prompt_max_length: number;
  /** false 时前端不显示排队位次（该模型后端无队列信息） */
  queue_position_available: boolean;
}

/* ─────────────────────────── 提交与错误（前端诉求） ─────────────────────────── */

/** POST /api/tasks 的请求体。注意：没有 mode 字段，隐式分流由后端按 inputs 判定 */
export interface CreateTaskRequest {
  model_id: string;
  prompt: string;
  /** key 为 InputSlotSpec.key，值为 upload_id 或已有 asset_id */
  inputs: Record<string, string[]>;
  /** key 为 ParamSpec.key（compound 已平铺），只包含 visible_when 为真的参数 */
  params: Record<string, JsonValue>;
  /** 前端生成的幂等键，断网重发不会产生两个任务 */
  client_token: string;
  /** 画布内发起时携带，完成后产物直接落到该画布 */
  canvas_id?: string;
}

export type TaskStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'canceled';

/**
 * 失败分类。前端严格按 code 呈现，不解析 message。
 * charged 一律为 false（PRD：三类失败都不扣费），但仍显式回传，前端据此显示「未扣费」。
 */
export type TaskErrorCode =
  | 'invalid_param'          // 参数非法 → 红色，回填并高亮出错芯片，可修正重试
  | 'upstream_rate_limited'  // 上游限流 → 黄色，显示自动重试进度，不给手动重试按钮
  | 'content_rejected'       // 审核拒绝 → 灰色，引导改提示词，不提供直接重试
  | 'insufficient_credit'    // 积分不足 → 引导去个人中心
  | 'internal_error';        // 兜底 → 可重试

export interface TaskError {
  code: TaskErrorCode;
  message: string;
  /** 仅 invalid_param 携带，key 对应 ParamSpec.key 或 InputSlotSpec.key */
  field_errors?: { key: string; message: string }[];
  retryable: boolean;
  charged: boolean;
  /** upstream_rate_limited 时携带，用于展示「正在自动重试 (2/5)」 */
  retry?: { attempt: number; max_attempts: number; next_at?: string };
}
