import type { JsonValue, ModelCapabilitySchema } from './types';
import { evaluate, type FormValues, type InputValues } from './evaluate';

export interface CostBreakdown {
  base: number;
  applied: { label: string; op: 'mul' | 'add'; value: number }[];
  multiplier: { key: string; value: number } | null;
  total: number;
}

/**
 * 本地算价（frontend-design.md §3.4）。只用于提交前的即时展示 ——
 * POST /api/tasks 返回的 estimated_cost 才是权威，不一致时静默以后端为准。
 */
export function costBreakdown(
  model: ModelCapabilitySchema,
  values: FormValues,
  inputs: InputValues,
): CostBreakdown {
  const pricing = model.pricing;
  let subtotal = pricing.base;
  const applied: CostBreakdown['applied'] = [];

  for (const m of pricing.modifiers) {
    if (!evaluate(m.when, values, inputs)) continue;
    subtotal = m.op === 'mul' ? subtotal * m.value : subtotal + m.value;
    applied.push({ label: m.label ?? '', op: m.op, value: m.value });
  }

  let multiplier: CostBreakdown['multiplier'] = null;
  if (pricing.multiplier_param) {
    const raw = Number(values[pricing.multiplier_param]);
    if (Number.isFinite(raw)) {
      subtotal *= raw;
      multiplier = { key: pricing.multiplier_param, value: raw };
    }
  }

  const round =
    pricing.rounding === 'floor' ? Math.floor : pricing.rounding === 'round' ? Math.round : Math.ceil;

  return { base: pricing.base, applied, multiplier, total: round(subtotal) };
}

export function estimateCost(
  model: ModelCapabilitySchema,
  values: Record<string, JsonValue>,
  inputs: InputValues | Record<string, string[]>,
): number {
  const normalized = Object.fromEntries(
    Object.entries(inputs).map(([k, v]) => [k, (v as unknown[]).map(() => ({ upload_id: '', preview_url: '', name: '' }))]),
  ) as InputValues;
  return costBreakdown(model, values, normalized).total;
}

/** eta.scales_with 指向某个数值参数时，按 当前值 / 默认值 线性缩放 */
export function estimateSeconds(model: ModelCapabilitySchema, values: FormValues): number {
  const { p50_seconds, scales_with } = model.eta;
  if (!scales_with) return p50_seconds;

  const findDefault = (key: string): number | null => {
    const stack = [...model.params];
    while (stack.length) {
      const spec = stack.pop()!;
      if (spec.control === 'compound') stack.push(...spec.fields);
      else if (spec.key === key) return Number(spec.default);
    }
    return null;
  };

  const current = Number(values[scales_with]);
  const base = findDefault(scales_with);
  if (!Number.isFinite(current) || !base) return p50_seconds;
  return Math.round((p50_seconds * current) / base);
}

export function formatSeconds(seconds: number): string {
  if (seconds < 60) return `约 ${seconds}s`;
  const min = Math.round(seconds / 60);
  return `约 ${min}min`;
}
