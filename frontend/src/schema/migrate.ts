import type { ModelCapabilitySchema } from './types';
import type { FormValues } from './evaluate';
import { leafSpecs } from './form';
import { isValidValue } from './validate';

export interface MigrationResult {
  values: FormValues;
  /** 被回落到 default 的 key，芯片据此高亮 1.5s，让用户看见哪些被改了 */
  migrated: string[];
}

/**
 * 切模型时的表单迁移（frontend-design.md §2.4）：
 * 同名 key 且在新约束下合法 → 保留；否则回落 default 并记账。
 * 用户挑参数是有成本的，切模型不该无声清空。
 */
export function migrateValues(next: ModelCapabilitySchema, previous: FormValues): MigrationResult {
  const values: FormValues = {};
  const migrated: string[] = [];

  for (const spec of leafSpecs(next.params)) {
    const had = Object.prototype.hasOwnProperty.call(previous, spec.key);
    if (had && isValidValue(spec, previous[spec.key])) {
      values[spec.key] = previous[spec.key];
    } else {
      values[spec.key] = spec.default;
      if (had) migrated.push(spec.key);
    }
  }

  return { values, migrated };
}

/** 「做同款」/「单卡重跑」：把一份历史参数灌回表单，同样走合法性回落 */
export function adoptValues(
  model: ModelCapabilitySchema,
  source: Record<string, unknown>,
): FormValues {
  const values: FormValues = {};
  for (const spec of leafSpecs(model.params)) {
    const candidate = source[spec.key];
    values[spec.key] = isValidValue(spec, candidate) ? (candidate as FormValues[string]) : spec.default;
  }
  return values;
}
