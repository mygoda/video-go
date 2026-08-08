import { useState } from 'react';
import type { AspectOption, AspectSelectSpec, JsonValue, ParamSpec } from '@/schema/types';
import type { FormValues, InputValues } from '@/schema/evaluate';
import { isCompound, isEnabled } from '@/schema/form';
import { fallbackOptions, isKnownControl } from '@/schema/controls';
import { Popover } from '../Popover';
import { ParamEditor, chipSummary } from './ParamEditor';

interface ParamChipProps {
  spec: ParamSpec;
  values: FormValues;
  inputs: InputValues;
  error?: string;
  migrated?: boolean;
  onChange(key: string, value: JsonValue): void;
}

function findAspectSpec(spec: ParamSpec): AspectSelectSpec | null {
  if (spec.control === 'aspect_select') return spec;
  if (isCompound(spec)) {
    for (const f of spec.fields) {
      const found = findAspectSpec(f);
      if (found) return found;
    }
  }
  return null;
}

/** 芯片上的等比小方块：从 aspect 选项的 ratio_w/h 算，不硬编码任何比例 */
function RatioBox({ spec, values }: { spec: AspectSelectSpec; values: FormValues }) {
  const option = spec.options.find((o) => o.value === values[spec.key]) as AspectOption | undefined;
  if (!option) return null;
  const long = 16;
  const scale = long / Math.max(option.ratio_w, option.ratio_h);
  return (
    <span
      className="ratio-box"
      style={{ width: Math.round(option.ratio_w * scale), height: Math.round(option.ratio_h * scale) }}
    />
  );
}

export function ParamChip({ spec, values, inputs, error, migrated, onChange }: ParamChipProps) {
  const [open, setOpen] = useState(false);
  const disabled = !isEnabled(spec, values, inputs);
  const readOnly = !isKnownControl(spec) && !fallbackOptions(spec);
  const aspect = findAspectSpec(spec);

  const className = [
    'chip',
    error ? 'error' : '',
    migrated ? 'migrated' : '',
  ]
    .filter(Boolean)
    .join(' ');

  // toggle 不值得开弹层：点一下就该翻过去
  if (spec.control === 'toggle') {
    return (
      <button
        type="button"
        className={className}
        role="switch"
        aria-checked={Boolean(values[spec.key])}
        disabled={disabled}
        title={disabled ? spec.disabled_hint : spec.help}
        onClick={() => onChange(spec.key, !values[spec.key])}
      >
        <span className="k">{spec.label}</span>
        <span className={`switch-track${values[spec.key] ? ' on' : ''}`}>
          <span className="switch-knob" />
        </span>
      </button>
    );
  }

  const trigger = (
    <button
      type="button"
      className={className}
      aria-haspopup={readOnly ? undefined : 'listbox'}
      aria-expanded={readOnly ? undefined : open}
      disabled={disabled || readOnly}
      title={disabled ? spec.disabled_hint : readOnly ? '该参数由模型提供，当前版本按默认值提交' : spec.help}
      onClick={() => setOpen((v) => !v)}
    >
      {aspect ? <RatioBox spec={aspect} values={values} /> : <span className="k">{spec.label}</span>}
      <span className="v">{chipSummary(spec, values)}</span>
      {!readOnly && (
        <span className="caret" aria-hidden="true">
          ▾
        </span>
      )}
    </button>
  );

  return (
    <Popover open={open && !readOnly} onClose={() => setOpen(false)} trigger={trigger}>
      <div className="popover-title">{spec.label}</div>
      <ParamEditor spec={spec} values={values} inputs={inputs} onChange={onChange} />
      {error && <p className="field-error">{error}</p>}
      {spec.help && <p className="hint" style={{ padding: '0 var(--space-2) var(--space-1)' }}>{spec.help}</p>}
    </Popover>
  );
}
