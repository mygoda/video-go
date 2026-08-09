import type {
  AspectOption,
  Condition,
  ControlKind,
  InputSlotSpec,
  JsonValue,
  ModelCapabilitySchema,
  OptionItem,
  ParamSpec,
  PricingModifier,
} from '@/schema/types';
import { ConditionEditor, DEFAULT_CONDITION } from './ConditionEditor';

/* ─────────────────────────── 通用小字段 ─────────────────────────── */

interface FieldProps {
  id: string;
  label: string;
  hint?: string;
  className?: string;
}

function TextField({
  id,
  label,
  hint,
  className,
  value,
  onChange,
  placeholder,
}: FieldProps & { value: string; onChange(v: string): void; placeholder?: string }) {
  return (
    <div className={`field ${className ?? ''}`}>
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        className="input"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
      />
      {hint && <p className="hint">{hint}</p>}
    </div>
  );
}

function NumberField({
  id,
  label,
  hint,
  className,
  value,
  onChange,
  min,
}: FieldProps & { value: number; onChange(v: number): void; min?: number }) {
  return (
    <div className={`field ${className ?? ''}`}>
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        className="input mono"
        type="number"
        min={min}
        value={value}
        onChange={(e) => onChange(Number(e.target.value))}
      />
      {hint && <p className="hint">{hint}</p>}
    </div>
  );
}

function SelectField<T extends string>({
  id,
  label,
  hint,
  className,
  value,
  onChange,
  options,
}: FieldProps & { value: T; onChange(v: T): void; options: { value: T; label: string }[] }) {
  return (
    <div className={`field ${className ?? ''}`}>
      <label htmlFor={id}>{label}</label>
      <select id={id} className="select" value={value} onChange={(e) => onChange(e.target.value as T)}>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {hint && <p className="hint">{hint}</p>}
    </div>
  );
}

function CheckField({
  id,
  label,
  checked,
  onChange,
}: {
  id: string;
  label: string;
  checked: boolean;
  onChange(v: boolean): void;
}) {
  return (
    <div className="check-row">
      <input id={id} type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      <label htmlFor={id}>{label}</label>
    </div>
  );
}

/** 可选条件：勾上才写进 schema，不勾就整个字段不存在 */
function OptionalCondition({
  id,
  label,
  value,
  onChange,
}: {
  id: string;
  label: string;
  value: Condition | undefined;
  onChange(v: Condition | undefined): void;
}) {
  return (
    <div className="field">
      <CheckField
        id={`${id}-on`}
        label={label}
        checked={value !== undefined}
        onChange={(on) => onChange(on ? DEFAULT_CONDITION : undefined)}
      />
      {value !== undefined && <ConditionEditor idPrefix={id} value={value} onChange={onChange} />}
    </div>
  );
}

/* ─────────────────────────── 选项列表 ─────────────────────────── */

function OptionsEditor({
  idPrefix,
  options,
  aspect,
  onChange,
}: {
  idPrefix: string;
  options: OptionItem[];
  aspect: boolean;
  onChange(next: OptionItem[]): void;
}) {
  function patch(index: number, next: Partial<AspectOption>): void {
    onChange(options.map((o, i) => (i === index ? { ...o, ...next } : o)));
  }

  return (
    <div className="editor-section">
      <h4>
        选项
        <span className="spacer" />
        <button
          type="button"
          className="btn btn-sm"
          onClick={() =>
            onChange([
              ...options,
              aspect
                ? ({ value: '', label: '', ratio_w: 1, ratio_h: 1 } as AspectOption)
                : { value: '', label: '' },
            ])
          }
        >
          添加选项
        </button>
      </h4>

      {options.map((option, i) => {
        const aspectOption = option as AspectOption;
        return (
          <div className="repeat-row" key={i}>
            <div className="repeat-head">
              <span>选项 {i + 1}</span>
              <span className="spacer" />
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={() => onChange(options.filter((_, j) => j !== i))}
              >
                删除
              </button>
            </div>
            <div className="form-grid">
              <TextField
                id={`${idPrefix}-opt-${i}-value`}
                label="提交值"
                value={String(option.value ?? '')}
                onChange={(v) => patch(i, { value: v })}
              />
              <TextField
                id={`${idPrefix}-opt-${i}-label`}
                label="展示名"
                value={option.label}
                onChange={(v) => patch(i, { label: v })}
              />
              <TextField
                id={`${idPrefix}-opt-${i}-badge`}
                label="角标"
                placeholder="如 +2 积分"
                value={option.badge ?? ''}
                onChange={(v) => patch(i, { badge: v || undefined })}
              />
              {aspect && (
                <>
                  <NumberField
                    id={`${idPrefix}-opt-${i}-rw`}
                    label="比例宽"
                    min={1}
                    value={aspectOption.ratio_w ?? 1}
                    onChange={(v) => patch(i, { ratio_w: v })}
                  />
                  <NumberField
                    id={`${idPrefix}-opt-${i}-rh`}
                    label="比例高"
                    min={1}
                    value={aspectOption.ratio_h ?? 1}
                    onChange={(v) => patch(i, { ratio_h: v })}
                  />
                </>
              )}
            </div>
            <CheckField
              id={`${idPrefix}-opt-${i}-disabled`}
              label="禁用该选项"
              checked={Boolean(option.disabled)}
              onChange={(v) => patch(i, { disabled: v || undefined })}
            />
            {option.disabled && (
              <TextField
                id={`${idPrefix}-opt-${i}-reason`}
                label="禁用理由"
                hint="禁用态旁必须给一行达标的理由文字，否则用户只看到一个点不动的选项"
                value={option.disabled_reason ?? ''}
                onChange={(v) => patch(i, { disabled_reason: v || undefined })}
              />
            )}
          </div>
        );
      })}

      {!options.length && <p className="hint">还没有选项，这个控件会渲染成空下拉。</p>}
    </div>
  );
}

/* ─────────────────────────── 参数 ─────────────────────────── */

const CONTROL_KINDS: { value: ControlKind; label: string }[] = [
  { value: 'select', label: '下拉 / 分段' },
  { value: 'aspect_select', label: '比例选择' },
  { value: 'compound', label: '复合芯片' },
  { value: 'stepper', label: '步进器' },
  { value: 'slider', label: '滑杆' },
  { value: 'toggle', label: '开关' },
  { value: 'seed', label: '种子' },
  { value: 'textarea', label: '多行文本' },
];

/** 换控件类型时保留通用字段，只重建该控件独有的部分 */
function convertParam(control: ControlKind, current: ParamSpec): ParamSpec {
  const base = {
    key: current.key,
    label: current.label,
    group: current.group,
    order: current.order,
    help: current.help,
    visible_when: current.visible_when,
    enabled_when: current.enabled_when,
    disabled_hint: current.disabled_hint,
  };
  const options: OptionItem[] = 'options' in current ? current.options : [];
  const numericDefault = typeof current.default === 'number' ? current.default : 0;

  switch (control) {
    case 'select':
      return { ...base, control, default: current.default ?? '', options };
    case 'aspect_select':
      return {
        ...base,
        control,
        default: current.default ?? '',
        options: options.map((o) => ({ ...o, ratio_w: (o as AspectOption).ratio_w ?? 1, ratio_h: (o as AspectOption).ratio_h ?? 1 })),
      };
    case 'compound':
      return {
        ...base,
        control,
        default: null,
        fields: 'fields' in current ? current.fields : [],
        display_template: 'display_template' in current ? current.display_template : '',
      };
    case 'stepper':
      return { ...base, control, default: numericDefault, min: 1, max: 8, step: 1 };
    case 'slider':
      return { ...base, control, default: numericDefault, min: 0, max: 1, step: 0.05 };
    case 'toggle':
      return { ...base, control, default: current.default === true };
    case 'seed':
      return { ...base, control, default: null, min: 0, max: 2 ** 31 - 1, allow_random: true };
    case 'textarea':
      return { ...base, control, default: current.default ?? '', max_length: 500 };
  }
}

export function newParam(order: number): ParamSpec {
  return { key: '', label: '', control: 'select', group: 'primary', order, default: '', options: [] };
}

function ParamRow({
  idPrefix,
  param,
  onChange,
  onRemove,
  allowCompound,
}: {
  idPrefix: string;
  param: ParamSpec;
  onChange(next: ParamSpec): void;
  onRemove(): void;
  allowCompound: boolean;
}) {
  const kinds = allowCompound ? CONTROL_KINDS : CONTROL_KINDS.filter((k) => k.value !== 'compound');

  return (
    <div className="repeat-row">
      <div className="repeat-head">
        <span className="tag tone-accent">{param.key || '未命名参数'}</span>
        <span className="spacer" />
        <button type="button" className="btn btn-sm btn-ghost" onClick={onRemove}>
          删除
        </button>
      </div>

      <div className="form-grid">
        <TextField
          id={`${idPrefix}-key`}
          label="提交 key"
          value={param.key}
          onChange={(v) => onChange({ ...param, key: v })}
        />
        <TextField
          id={`${idPrefix}-label`}
          label="展示名"
          value={param.label}
          onChange={(v) => onChange({ ...param, label: v })}
        />
        <SelectField
          id={`${idPrefix}-control`}
          label="控件"
          value={param.control}
          options={kinds}
          onChange={(v) => onChange(convertParam(v, param))}
        />
        <SelectField
          id={`${idPrefix}-group`}
          label="分组"
          value={param.group}
          options={[
            { value: 'primary', label: '主参数芯片条' },
            { value: 'advanced', label: '⚙ 折叠面板' },
          ]}
          onChange={(v) => onChange({ ...param, group: v })}
        />
        <NumberField
          id={`${idPrefix}-order`}
          label="排序"
          value={param.order}
          onChange={(v) => onChange({ ...param, order: v })}
        />
        {param.control !== 'compound' && param.control !== 'seed' && (
          <TextField
            id={`${idPrefix}-default`}
            label="默认值"
            value={param.default === null ? '' : String(param.default)}
            onChange={(v) =>
              onChange({
                ...param,
                default: param.control === 'toggle' ? v === 'true' : coerceDefault(v),
              } as ParamSpec)
            }
          />
        )}
        <TextField
          className="span-2"
          id={`${idPrefix}-help`}
          label="说明"
          value={param.help ?? ''}
          onChange={(v) => onChange({ ...param, help: v || undefined })}
        />
      </div>

      {(param.control === 'stepper' || param.control === 'slider') && (
        <div className="form-grid">
          <NumberField
            id={`${idPrefix}-min`}
            label="最小值"
            value={param.min}
            onChange={(v) => onChange({ ...param, min: v })}
          />
          <NumberField
            id={`${idPrefix}-max`}
            label="最大值"
            value={param.max}
            onChange={(v) => onChange({ ...param, max: v })}
          />
          <NumberField
            id={`${idPrefix}-step`}
            label="步长"
            value={param.step}
            onChange={(v) => onChange({ ...param, step: v })}
          />
          <TextField
            id={`${idPrefix}-unit`}
            label="单位"
            value={param.unit ?? ''}
            onChange={(v) => onChange({ ...param, unit: v || undefined })}
          />
        </div>
      )}

      {param.control === 'seed' && (
        <div className="form-grid">
          <NumberField
            id={`${idPrefix}-min`}
            label="最小值"
            value={param.min}
            onChange={(v) => onChange({ ...param, min: v })}
          />
          <NumberField
            id={`${idPrefix}-max`}
            label="最大值"
            value={param.max}
            onChange={(v) => onChange({ ...param, max: v })}
          />
          <div className="span-2">
            <CheckField
              id={`${idPrefix}-random`}
              label="允许随机（芯片上出现 🎲）"
              checked={param.allow_random}
              onChange={(v) => onChange({ ...param, allow_random: v })}
            />
          </div>
        </div>
      )}

      {param.control === 'textarea' && (
        <div className="form-grid">
          <NumberField
            id={`${idPrefix}-maxlen`}
            label="最大长度"
            value={param.max_length}
            onChange={(v) => onChange({ ...param, max_length: v })}
          />
          <TextField
            id={`${idPrefix}-placeholder`}
            label="占位文案"
            value={param.placeholder ?? ''}
            onChange={(v) => onChange({ ...param, placeholder: v || undefined })}
          />
        </div>
      )}

      {(param.control === 'select' || param.control === 'aspect_select') && (
        <OptionsEditor
          idPrefix={idPrefix}
          aspect={param.control === 'aspect_select'}
          options={param.options}
          onChange={(options) =>
            param.control === 'aspect_select'
              ? onChange({ ...param, options: options as AspectOption[] })
              : onChange({ ...param, options })
          }
        />
      )}

      {param.control === 'compound' && (
        <div className="editor-section">
          <h4>
            子字段
            <span className="spacer" />
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => onChange({ ...param, fields: [...param.fields, newParam(param.fields.length)] })}
            >
              添加子字段
            </button>
          </h4>
          <TextField
            id={`${idPrefix}-template`}
            label="芯片标签模板"
            placeholder="{aspect} · {count}张"
            hint="用 {字段key} 占位"
            value={param.display_template}
            onChange={(v) => onChange({ ...param, display_template: v })}
          />
          {param.fields.map((field, i) => (
            <ParamRow
              key={i}
              idPrefix={`${idPrefix}-f${i}`}
              param={field}
              allowCompound={false}
              onChange={(next) =>
                onChange({ ...param, fields: param.fields.map((f, j) => (j === i ? next : f)) })
              }
              onRemove={() => onChange({ ...param, fields: param.fields.filter((_, j) => j !== i) })}
            />
          ))}
        </div>
      )}

      <OptionalCondition
        id={`${idPrefix}-visible`}
        label="仅在满足条件时显示"
        value={param.visible_when}
        onChange={(v) => onChange({ ...param, visible_when: v })}
      />
      <OptionalCondition
        id={`${idPrefix}-enabled`}
        label="仅在满足条件时可用"
        value={param.enabled_when}
        onChange={(v) => onChange({ ...param, enabled_when: v })}
      />
      {param.enabled_when && (
        <TextField
          id={`${idPrefix}-disabled-hint`}
          label="禁用提示"
          value={param.disabled_hint ?? ''}
          onChange={(v) => onChange({ ...param, disabled_hint: v || undefined })}
        />
      )}
    </div>
  );
}

function coerceDefault(text: string): JsonValue {
  if (text === '') return '';
  if (text === 'true') return true;
  if (text === 'false') return false;
  if (!Number.isNaN(Number(text))) return Number(text);
  return text;
}

/* ─────────────────────────── 输入槽 ─────────────────────────── */

function InputSlotRow({
  idPrefix,
  slot,
  onChange,
  onRemove,
}: {
  idPrefix: string;
  slot: InputSlotSpec;
  onChange(next: InputSlotSpec): void;
  onRemove(): void;
}) {
  return (
    <div className="repeat-row">
      <div className="repeat-head">
        <span className="tag tone-accent">{slot.key || '未命名槽'}</span>
        <span className="spacer" />
        <button type="button" className="btn btn-sm btn-ghost" onClick={onRemove}>
          删除
        </button>
      </div>
      <div className="form-grid">
        <TextField
          id={`${idPrefix}-key`}
          label="提交 key"
          value={slot.key}
          onChange={(v) => onChange({ ...slot, key: v })}
        />
        <TextField
          id={`${idPrefix}-label`}
          label="上传位文案"
          value={slot.label}
          onChange={(v) => onChange({ ...slot, label: v })}
        />
        <SelectField
          id={`${idPrefix}-kind`}
          label="文件类型"
          value={slot.kind}
          options={[
            { value: 'image', label: '图片' },
            { value: 'video', label: '视频' },
            { value: 'audio', label: '音频' },
          ]}
          onChange={(v) => onChange({ ...slot, kind: v })}
        />
        <TextField
          id={`${idPrefix}-accept`}
          label="MIME 白名单"
          placeholder="image/png,image/jpeg"
          value={slot.accept.join(',')}
          onChange={(v) =>
            onChange({ ...slot, accept: v.split(',').map((s) => s.trim()).filter(Boolean) })
          }
        />
        <NumberField
          id={`${idPrefix}-min`}
          label="最少数量"
          min={0}
          value={slot.min_count}
          onChange={(v) => onChange({ ...slot, min_count: v })}
        />
        <NumberField
          id={`${idPrefix}-max`}
          label="最多数量"
          min={1}
          value={slot.max_count}
          onChange={(v) => onChange({ ...slot, max_count: v })}
        />
        <NumberField
          id={`${idPrefix}-bytes`}
          label="单文件上限（字节）"
          min={1}
          value={slot.max_bytes}
          onChange={(v) => onChange({ ...slot, max_bytes: v })}
        />
        <TextField
          id={`${idPrefix}-hint`}
          label="悬浮提示"
          value={slot.hint ?? ''}
          onChange={(v) => onChange({ ...slot, hint: v || undefined })}
        />
      </div>
      <CheckField
        id={`${idPrefix}-required`}
        label="必填（不勾 = 可选输入，也就是「传了图走图生图」这条隐式分流的分流点）"
        checked={slot.required}
        onChange={(v) => onChange({ ...slot, required: v })}
      />
    </div>
  );
}

/* ─────────────────────────── 顶层 ─────────────────────────── */

export function CapabilityEditor({
  value,
  onChange,
}: {
  value: ModelCapabilitySchema;
  onChange(next: ModelCapabilitySchema): void;
}) {
  function patchPricing(next: Partial<ModelCapabilitySchema['pricing']>): void {
    onChange({ ...value, pricing: { ...value.pricing, ...next } });
  }

  function patchModifier(index: number, next: PricingModifier): void {
    patchPricing({ modifiers: value.pricing.modifiers.map((m, i) => (i === index ? next : m)) });
  }

  return (
    <>
      <div className="editor-section">
        <h4>基本信息</h4>
        <div className="form-grid">
          <TextField
            id="cap-name"
            label="展示名"
            hint="用户在模型选择器里看到的名字"
            value={value.name}
            onChange={(v) => onChange({ ...value, name: v })}
          />
          <TextField
            id="cap-vendor"
            label="供应商展示名"
            hint="仅用于展示与故障提示，前端不按它分支"
            value={value.vendor}
            onChange={(v) => onChange({ ...value, vendor: v })}
          />
          <SelectField
            id="cap-modality"
            label="模态"
            value={value.modality}
            options={[
              { value: 'image', label: '图片' },
              { value: 'video', label: '视频' },
              { value: 'text', label: '文本（仅平台内部，不进用户目录）' },
            ]}
            onChange={(v) => onChange({ ...value, modality: v })}
          />
          <NumberField
            id="cap-order"
            label="排序权重"
            hint="小的在前"
            value={value.order}
            onChange={(v) => onChange({ ...value, order: v })}
          />
          <TextField
            className="span-2"
            id="cap-desc"
            label="简介"
            value={value.description ?? ''}
            onChange={(v) => onChange({ ...value, description: v || undefined })}
          />
          <TextField
            className="span-2"
            id="cap-preview"
            label="缩略示例图 URL"
            value={value.preview_url ?? ''}
            onChange={(v) => onChange({ ...value, preview_url: v || undefined })}
          />
        </div>
        <CheckField
          id="cap-enabled"
          label="对用户可选"
          checked={value.enabled}
          onChange={(v) => onChange({ ...value, enabled: v })}
        />
        {!value.enabled && (
          <TextField
            id="cap-disabled-reason"
            label="置灰理由"
            hint="禁用态必须配一行说明，否则用户只看到一个点不动的模型"
            value={value.disabled_reason ?? ''}
            onChange={(v) => onChange({ ...value, disabled_reason: v || undefined })}
          />
        )}
      </div>

      <div className="editor-section">
        <h4>
          输入槽
          <span className="spacer" />
          <button
            type="button"
            className="btn btn-sm"
            onClick={() =>
              onChange({
                ...value,
                inputs: [
                  ...value.inputs,
                  {
                    key: '',
                    label: '',
                    kind: 'image',
                    required: false,
                    min_count: 0,
                    max_count: 1,
                    accept: ['image/png', 'image/jpeg'],
                    max_bytes: 10 * 1024 ** 2,
                  },
                ],
              })
            }
          >
            添加输入槽
          </button>
        </h4>
        {value.inputs.map((slot, i) => (
          <InputSlotRow
            key={i}
            idPrefix={`slot-${i}`}
            slot={slot}
            onChange={(next) => onChange({ ...value, inputs: value.inputs.map((s, j) => (j === i ? next : s)) })}
            onRemove={() => onChange({ ...value, inputs: value.inputs.filter((_, j) => j !== i) })}
          />
        ))}
        {!value.inputs.length && <p className="hint">没有输入槽 = 纯文生模型。</p>}
      </div>

      <div className="editor-section">
        <h4>
          参数
          <span className="spacer" />
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => onChange({ ...value, params: [...value.params, newParam(value.params.length)] })}
          >
            添加参数
          </button>
        </h4>
        {value.params.map((param, i) => (
          <ParamRow
            key={i}
            idPrefix={`param-${i}`}
            param={param}
            allowCompound
            onChange={(next) => onChange({ ...value, params: value.params.map((p, j) => (j === i ? next : p)) })}
            onRemove={() => onChange({ ...value, params: value.params.filter((_, j) => j !== i) })}
          />
        ))}
        {!value.params.length && <p className="hint">没有参数 = 芯片栏只有模型芯片。</p>}
      </div>

      <div className="editor-section">
        <h4>计费</h4>
        <div className="form-grid">
          <NumberField
            id="cap-base"
            label="基础积分"
            min={0}
            value={value.pricing.base}
            onChange={(v) => patchPricing({ base: v })}
          />
          <SelectField
            id="cap-rounding"
            label="取整方式"
            value={value.pricing.rounding}
            options={[
              { value: 'ceil', label: '向上取整' },
              { value: 'round', label: '四舍五入' },
              { value: 'floor', label: '向下取整' },
            ]}
            onChange={(v) => patchPricing({ rounding: v })}
          />
          <TextField
            className="span-2"
            id="cap-multiplier"
            label="倍数参数 key"
            hint="最后乘上该参数的数值，如 count（几张）或 duration（几秒）。留空表示不乘"
            value={value.pricing.multiplier_param ?? ''}
            onChange={(v) => patchPricing({ multiplier_param: v || undefined })}
          />
        </div>

        <h4>
          加价规则
          <span className="spacer" />
          <button
            type="button"
            className="btn btn-sm"
            onClick={() =>
              patchPricing({
                modifiers: [...value.pricing.modifiers, { when: DEFAULT_CONDITION, op: 'add', value: 0 }],
              })
            }
          >
            添加规则
          </button>
        </h4>
        {value.pricing.modifiers.map((modifier, i) => (
          <div className="repeat-row" key={i}>
            <div className="repeat-head">
              <span>规则 {i + 1}</span>
              <span className="spacer" />
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={() =>
                  patchPricing({ modifiers: value.pricing.modifiers.filter((_, j) => j !== i) })
                }
              >
                删除
              </button>
            </div>
            <div className="form-grid">
              <SelectField
                id={`mod-${i}-op`}
                label="运算"
                value={modifier.op}
                options={[
                  { value: 'add', label: '加' },
                  { value: 'mul', label: '乘' },
                ]}
                onChange={(v) => patchModifier(i, { ...modifier, op: v })}
              />
              <NumberField
                id={`mod-${i}-value`}
                label="数值"
                value={modifier.value}
                onChange={(v) => patchModifier(i, { ...modifier, value: v })}
              />
              <TextField
                className="span-2"
                id={`mod-${i}-label`}
                label="展示文案"
                placeholder="如 1080P 加价"
                value={modifier.label ?? ''}
                onChange={(v) => patchModifier(i, { ...modifier, label: v || undefined })}
              />
            </div>
            <div className="field">
              <span className="hint">命中条件</span>
              <ConditionEditor
                idPrefix={`mod-${i}`}
                value={modifier.when}
                onChange={(when) => patchModifier(i, { ...modifier, when })}
              />
            </div>
          </div>
        ))}
        {!value.pricing.modifiers.length && <p className="hint">没有加价规则，费用就是基础积分乘倍数参数。</p>}
      </div>

      <div className="editor-section">
        <h4>耗时与限制</h4>
        <div className="form-grid">
          <NumberField
            id="cap-p50"
            label="p50 秒"
            min={1}
            value={value.eta.p50_seconds}
            onChange={(v) => onChange({ ...value, eta: { ...value.eta, p50_seconds: v } })}
          />
          <NumberField
            id="cap-p90"
            label="p90 秒"
            min={1}
            value={value.eta.p90_seconds}
            onChange={(v) => onChange({ ...value, eta: { ...value.eta, p90_seconds: v } })}
          />
          <TextField
            className="span-2"
            id="cap-scales"
            label="耗时随哪个参数线性缩放"
            hint="留空表示固定耗时"
            value={value.eta.scales_with ?? ''}
            onChange={(v) => onChange({ ...value, eta: { ...value.eta, scales_with: v || undefined } })}
          />
          <NumberField
            id="cap-concurrent"
            label="单用户并发上限"
            min={1}
            value={value.limits.max_concurrent_per_user}
            onChange={(v) =>
              onChange({ ...value, limits: { ...value.limits, max_concurrent_per_user: v } })
            }
          />
          <NumberField
            id="cap-prompt-max"
            label="提示词最大长度"
            min={1}
            value={value.limits.prompt_max_length}
            onChange={(v) => onChange({ ...value, limits: { ...value.limits, prompt_max_length: v } })}
          />
        </div>
        <CheckField
          id="cap-queue"
          label="上游能给出排队位次"
          checked={value.limits.queue_position_available}
          onChange={(v) =>
            onChange({ ...value, limits: { ...value.limits, queue_position_available: v } })
          }
        />
      </div>
    </>
  );
}
