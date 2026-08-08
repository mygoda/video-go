import type { ControlKind, OptionItem, ParamSpec } from './types';

/**
 * 前端只认这 8 种 control（frontend-design.md §3.2）。
 * 未知 control 必须降级渲染、不允许白屏 —— 这是 schema 驱动能解耦前后端发版节奏的前提。
 */
const KNOWN_CONTROLS = new Set<ControlKind>([
  'select',
  'aspect_select',
  'compound',
  'stepper',
  'slider',
  'toggle',
  'seed',
  'textarea',
]);

export function isKnownControl(spec: ParamSpec): boolean {
  return KNOWN_CONTROLS.has(spec.control);
}

/** 未知 control 若带 options，就按 select 渲染；否则渲染成只读芯片 */
export function fallbackOptions(spec: ParamSpec): OptionItem[] | null {
  const options = (spec as { options?: OptionItem[] }).options;
  return Array.isArray(options) && options.length > 0 ? options : null;
}
