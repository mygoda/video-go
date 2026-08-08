import type { Condition, JsonPrimitive, JsonValue } from './types';

export type FormValues = Record<string, JsonValue>;

/** 每个输入槽当前挂着的文件。has_input 只关心「有没有」 */
export interface UploadedFile {
  upload_id: string;
  preview_url: string;
  name: string;
}
export type InputValues = Record<string, UploadedFile[]>;

/**
 * 条件表达式求值。参数联动与「传图 = 图生图」的隐式分流都靠它，
 * 加新模型不需要动这个函数 —— 这是「接新模型前端零改动」的地基之一。
 */
export function evaluate(cond: Condition | undefined, values: FormValues, inputs: InputValues): boolean {
  if (!cond) return true;

  switch (cond.op) {
    case 'eq':
      return values[cond.key] === cond.value;
    case 'ne':
      return values[cond.key] !== cond.value;
    case 'gt':
      return Number(values[cond.key]) > cond.value;
    case 'lt':
      return Number(values[cond.key]) < cond.value;
    case 'in':
      return cond.value.includes(values[cond.key] as JsonPrimitive);
    case 'nin':
      return !cond.value.includes(values[cond.key] as JsonPrimitive);
    case 'has_input':
      return (inputs[cond.slot]?.length ?? 0) > 0;
    case 'and':
      return cond.of.every((c) => evaluate(c, values, inputs));
    case 'or':
      return cond.of.some((c) => evaluate(c, values, inputs));
    case 'not':
      return !evaluate(cond.of, values, inputs);
    default:
      // 后端上了新算子而前端还没发版：当作恒真，不阻断渲染
      return true;
  }
}
