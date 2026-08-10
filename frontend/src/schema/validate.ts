import type { ModelCapabilitySchema, OptionItem, ParamSpec } from './types';
import type { FormValues, InputValues } from './evaluate';
import { isVisible, leafSpecs } from './form';
import { fallbackOptions, isKnownControl } from './controls';

export type FieldErrors = Record<string, string>;

function optionsOf(spec: ParamSpec): OptionItem[] | null {
  if (spec.control === 'select' || spec.control === 'aspect_select') return spec.options;
  return isKnownControl(spec) ? null : fallbackOptions(spec);
}

/** 单个参数值是否满足 schema 约束。切模型迁移与提交前校验共用这一份判定 */
export function isValidValue(spec: ParamSpec, value: unknown): boolean {
  const options = optionsOf(spec);
  if (options) return options.some((o) => o.value === value && !o.disabled);

  switch (spec.control) {
    case 'stepper':
    case 'slider': {
      const n = Number(value);
      return Number.isFinite(n) && n >= spec.min && n <= spec.max;
    }
    case 'seed':
      if (value === null) return true;
      return Number.isFinite(Number(value)) && Number(value) >= spec.min && Number(value) <= spec.max;
    case 'toggle':
      return typeof value === 'boolean';
    case 'textarea':
      return typeof value === 'string' && value.length <= spec.max_length;
    case 'compound':
      return true;
    default:
      // 未知 control 且无 options：只做透传，不判定
      return true;
  }
}

/**
 * 前端校验是体验不是安全边界（frontend-design.md §3.5）——
 * 后端仍会完整再校验一遍，这里只是不让用户盲提交。
 */
export function validateForm(
  model: ModelCapabilitySchema,
  prompt: string,
  values: FormValues,
  inputs: InputValues,
): FieldErrors {
  const errors: FieldErrors = {};

  if (prompt.length > model.limits.prompt_max_length) {
    errors.prompt = `提示词最长 ${model.limits.prompt_max_length} 字，当前 ${prompt.length}`;
  }

  for (const slot of model.inputs) {
    const files = inputs[slot.key] ?? [];
    if (slot.required && files.length < Math.max(1, slot.min_count)) {
      errors[slot.key] = `${slot.label}为必填`;
    } else if (files.length > slot.max_count) {
      errors[slot.key] = `${slot.label}最多 ${slot.max_count} 个`;
    } else if (slot.requires_slot && files.length > 0 && !(inputs[slot.requires_slot] ?? []).length) {
      const dep = model.inputs.find((s) => s.key === slot.requires_slot);
      errors[slot.key] = `${slot.label}需要同时提供${dep?.label ?? slot.requires_slot}`;
    }
  }

  for (const spec of leafSpecs(model.params)) {
    if (!isVisible(spec, values, inputs)) continue;
    if (!isValidValue(spec, values[spec.key])) {
      errors[spec.key] = `${spec.label}的取值不合法`;
    }
  }

  return errors;
}

/** 上传前的本地预校验，避免传完才报错 */
export async function validateFile(
  slot: ModelCapabilitySchema['inputs'][number],
  file: File,
): Promise<string | null> {
  if (slot.accept.length && !slot.accept.includes(file.type)) {
    return `仅支持 ${slot.accept.map((a) => a.replace(/^\w+\//, '')).join(' / ')}`;
  }
  if (file.size > slot.max_bytes) {
    return `文件不能超过 ${Math.round(slot.max_bytes / 1024 / 1024)}MB`;
  }
  if (slot.kind !== 'image' || (!slot.min_pixels && !slot.max_pixels)) return null;

  const size = await readImageSize(file);
  if (!size) return null;
  if (slot.min_pixels && (size.width < slot.min_pixels.width || size.height < slot.min_pixels.height)) {
    return `尺寸不能小于 ${slot.min_pixels.width}×${slot.min_pixels.height}`;
  }
  if (slot.max_pixels && (size.width > slot.max_pixels.width || size.height > slot.max_pixels.height)) {
    return `尺寸不能大于 ${slot.max_pixels.width}×${slot.max_pixels.height}`;
  }
  return null;
}

function readImageSize(file: File): Promise<{ width: number; height: number } | null> {
  return new Promise((resolve) => {
    const url = URL.createObjectURL(file);
    const img = new Image();
    img.onload = () => {
      URL.revokeObjectURL(url);
      resolve({ width: img.naturalWidth, height: img.naturalHeight });
    };
    img.onerror = () => {
      URL.revokeObjectURL(url);
      resolve(null);
    };
    img.src = url;
  });
}
