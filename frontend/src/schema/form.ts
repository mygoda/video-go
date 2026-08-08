import type { CompoundSpec, JsonValue, ModelCapabilitySchema, ParamSpec } from './types';
import { evaluate, type FormValues, type InputValues } from './evaluate';

export function isCompound(spec: ParamSpec): spec is CompoundSpec {
  return spec.control === 'compound';
}

/**
 * 展平后的叶子参数。compound 自身不产生 params key，它的 fields 各自平铺
 * （frontend-design.md §3.2 「compound 的提交语义要写死」）。
 */
export function leafSpecs(params: ParamSpec[]): ParamSpec[] {
  return params.flatMap((p) => (isCompound(p) ? leafSpecs(p.fields) : [p]));
}

export function defaultValues(model: ModelCapabilitySchema): FormValues {
  const values: FormValues = {};
  for (const spec of leafSpecs(model.params)) values[spec.key] = spec.default;
  return values;
}

/** compound 的子字段可见性 = 父可见 && 自身 visible_when 成立 */
export function isVisible(spec: ParamSpec, values: FormValues, inputs: InputValues): boolean {
  return evaluate(spec.visible_when, values, inputs);
}

export function isEnabled(spec: ParamSpec, values: FormValues, inputs: InputValues): boolean {
  return evaluate(spec.enabled_when, values, inputs);
}

export function sortByOrder(params: ParamSpec[]): ParamSpec[] {
  return [...params].sort((a, b) => a.order - b.order || a.label.localeCompare(b.label));
}

export function groupParams(
  model: ModelCapabilitySchema,
  values: FormValues,
  inputs: InputValues,
  group: 'primary' | 'advanced',
): ParamSpec[] {
  return sortByOrder(
    model.params.filter((p) => p.group === group && isVisible(p, values, inputs)),
  );
}

/**
 * 提交体里的 params：只包含 visible_when 为真的叶子参数。
 * 不可见的 key 必须从提交体里消失，否则「移除参考图后重绘强度还跟着提交」。
 */
export function submitParams(
  model: ModelCapabilitySchema,
  values: FormValues,
  inputs: InputValues,
): Record<string, JsonValue> {
  const out: Record<string, JsonValue> = {};
  const walk = (specs: ParamSpec[]) => {
    for (const spec of specs) {
      if (!isVisible(spec, values, inputs)) continue;
      if (isCompound(spec)) walk(spec.fields);
      else out[spec.key] = values[spec.key];
    }
  };
  walk(model.params);
  return out;
}

/** compound 的芯片文案：把 "{aspect} · {count}张" 里的占位替换成当前值 */
export function renderTemplate(spec: CompoundSpec, values: FormValues): string {
  return spec.display_template.replace(/\{(\w+)\}/g, (_, key: string) => String(values[key] ?? ''));
}
