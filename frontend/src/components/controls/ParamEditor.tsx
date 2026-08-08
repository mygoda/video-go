import type { AspectOption, JsonValue, OptionItem, ParamSpec } from '@/schema/types';
import type { FormValues, InputValues } from '@/schema/evaluate';
import { isCompound, isEnabled, isVisible, renderTemplate, sortByOrder } from '@/schema/form';
import { fallbackOptions, isKnownControl } from '@/schema/controls';

export interface EditorProps {
  spec: ParamSpec;
  values: FormValues;
  inputs: InputValues;
  onChange(key: string, value: JsonValue): void;
}

function labelOf(options: OptionItem[], value: JsonValue): string {
  return options.find((o) => o.value === value)?.label ?? String(value ?? '');
}

/** 芯片上显示的当前值。未知 control 也要给出一段可读文案，不能是 [object Object] */
export function chipSummary(spec: ParamSpec, values: FormValues): string {
  if (isCompound(spec)) return renderTemplate(spec, values);
  const value = values[spec.key];
  const custom = !isKnownControl(spec) ? fallbackOptions(spec) : null;
  if (custom) return labelOf(custom, value);

  switch (spec.control) {
    case 'select':
    case 'aspect_select':
      return labelOf(spec.options, value);
    case 'stepper':
    case 'slider':
      return `${value}${spec.unit ?? ''}`;
    case 'toggle':
      return value ? '开' : '关';
    case 'seed':
      return value === null ? '随机' : String(value);
    case 'textarea': {
      const text = String(value ?? '');
      if (!text) return '未设置';
      return text.length > 12 ? `${text.slice(0, 12)}…` : text;
    }
    default:
      return String(value ?? '—');
  }
}

function OptionList({
  options,
  value,
  onPick,
}: {
  options: OptionItem[];
  value: JsonValue;
  onPick(v: JsonValue): void;
}) {
  return (
    <div role="listbox" className="popover-scroll">
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          role="option"
          className="option"
          aria-selected={o.value === value}
          disabled={o.disabled}
          title={o.disabled ? o.disabled_reason : undefined}
          onClick={() => onPick(o.value)}
        >
          <span>{o.label}</span>
          {o.badge && <span className="sub">{o.badge}</span>}
          {o.value === value && <span className="check" aria-hidden="true">✓</span>}
        </button>
      ))}
    </div>
  );
}

function Segmented({
  options,
  value,
  onPick,
}: {
  options: OptionItem[];
  value: JsonValue;
  onPick(v: JsonValue): void;
}) {
  return (
    <div className="segmented">
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          aria-pressed={o.value === value}
          disabled={o.disabled}
          title={o.disabled ? o.disabled_reason : undefined}
          onClick={() => onPick(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

function ratioBoxStyle(o: AspectOption): { width: number; height: number } {
  const long = 16;
  const scale = long / Math.max(o.ratio_w, o.ratio_h);
  return { width: Math.round(o.ratio_w * scale), height: Math.round(o.ratio_h * scale) };
}

/** 一个参数的展开态编辑器。整个函数没有任何模型 id 分支——只看 control */
export function ParamEditor({ spec, values, inputs, onChange }: EditorProps) {
  const value = values[spec.key];
  const disabled = !isEnabled(spec, values, inputs);

  if (isCompound(spec)) {
    return (
      <>
        {sortByOrder(spec.fields)
          .filter((f) => isVisible(f, values, inputs))
          .map((field) => (
            <div key={field.key}>
              <div className="popover-title">{field.label}</div>
              <ParamEditor spec={field} values={values} inputs={inputs} onChange={onChange} />
            </div>
          ))}
      </>
    );
  }

  if (disabled) {
    return <p className="field-error">{spec.disabled_hint ?? '当前条件下不可调整'}</p>;
  }

  // 未知 control：带 options 就按 select 渲染，否则只读透传（frontend-design.md §3.2）
  if (!isKnownControl(spec)) {
    const options = fallbackOptions(spec);
    if (options) return <OptionList options={options} value={value} onPick={(v) => onChange(spec.key, v)} />;
    return (
      <p className="field-error" style={{ color: 'var(--text-tertiary)' }}>
        当前版本暂不支持编辑该参数，将按默认值提交
      </p>
    );
  }

  switch (spec.control) {
    case 'select':
      return spec.render_hint === 'segmented' ? (
        <Segmented options={spec.options} value={value} onPick={(v) => onChange(spec.key, v)} />
      ) : (
        <OptionList options={spec.options} value={value} onPick={(v) => onChange(spec.key, v)} />
      );

    case 'aspect_select':
      return (
        <div className="ratio-grid" role="listbox">
          {spec.options.map((o) => (
            <button
              key={String(o.value)}
              type="button"
              role="option"
              className="ratio-cell"
              aria-selected={o.value === value}
              disabled={o.disabled}
              onClick={() => onChange(spec.key, o.value)}
            >
              <span className="ratio-box" style={ratioBoxStyle(o)} />
              {o.label}
              {o.pixels && (
                <span style={{ color: 'var(--text-tertiary)' }}>
                  {o.pixels.width}×{o.pixels.height}
                </span>
              )}
            </button>
          ))}
        </div>
      );

    case 'stepper': {
      const n = Number(value);
      return (
        <div className="control-row">
          <button
            type="button"
            className="icon-btn"
            style={{ position: 'static' }}
            aria-label={`减少${spec.label}`}
            disabled={n <= spec.min}
            onClick={() => onChange(spec.key, Math.max(spec.min, n - spec.step))}
          >
            ⊖
          </button>
          <span className="mono" style={{ fontSize: 'var(--text-md)' }}>
            {n}
          </span>
          <button
            type="button"
            className="icon-btn"
            style={{ position: 'static' }}
            aria-label={`增加${spec.label}`}
            disabled={n >= spec.max}
            onClick={() => onChange(spec.key, Math.min(spec.max, n + spec.step))}
          >
            ⊕
          </button>
          <span className="hint" style={{ marginLeft: 'auto' }}>
            上限 {spec.max}
          </span>
        </div>
      );
    }

    case 'slider':
      return (
        <div className="control-row">
          <input
            type="range"
            min={spec.min}
            max={spec.max}
            step={spec.step}
            value={Number(value)}
            aria-label={spec.label}
            onChange={(e) => onChange(spec.key, Number(e.target.value))}
          />
          <span className="mono" style={{ minWidth: 36, textAlign: 'right' }}>
            {Number(value)}
            {spec.unit ?? ''}
          </span>
        </div>
      );

    case 'toggle':
      return (
        <div className="control-row">
          <button
            type="button"
            className="btn btn-sm"
            role="switch"
            aria-checked={Boolean(value)}
            onClick={() => onChange(spec.key, !value)}
          >
            <span className={`switch-track${value ? ' on' : ''}`}>
              <span className="switch-knob" />
            </span>
            {value ? '已开启' : '已关闭'}
          </button>
        </div>
      );

    case 'seed':
      return (
        <div className="control-row">
          <input
            className="num-input"
            type="number"
            min={spec.min}
            max={spec.max}
            aria-label={spec.label}
            placeholder="随机"
            value={value === null ? '' : Number(value)}
            onChange={(e) => onChange(spec.key, e.target.value === '' ? null : Number(e.target.value))}
          />
          {spec.allow_random && (
            <>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => onChange(spec.key, Math.floor(Math.random() * (spec.max - spec.min + 1)) + spec.min)}
              >
                🎲 随机一个
              </button>
              <button type="button" className="btn btn-sm" onClick={() => onChange(spec.key, null)}>
                每次随机
              </button>
            </>
          )}
        </div>
      );

    case 'textarea':
      return (
        <div style={{ padding: 'var(--space-2)' }}>
          <textarea
            className="textarea"
            maxLength={spec.max_length}
            placeholder={spec.placeholder}
            aria-label={spec.label}
            value={String(value ?? '')}
            onChange={(e) => onChange(spec.key, e.target.value)}
          />
          <p className="hint" style={{ textAlign: 'right' }}>
            {String(value ?? '').length}/{spec.max_length}
          </p>
        </div>
      );

    default:
      return null;
  }
}
