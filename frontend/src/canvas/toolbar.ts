import type { CanvasCard } from '@/api/types';
import type { Size } from './useElementSize';
import type { Viewport } from './useViewport';

/** 工具条与视口四边之间至少留这么多 */
const EDGE = 8;

function clamp(value: number, min: number, max: number): number {
  // 视口比工具条还小时区间是空的，此时贴上/左，至少让人看得见它
  if (max < min) return min;
  return Math.min(max, Math.max(min, value));
}

/**
 * 工具条在视口坐标系里的落点：默认贴着卡片上沿之上、右沿对齐。
 *
 * 关键是必须钳进视口。`.canvas-viewport` 是 overflow:hidden 的，坐标一旦出界，
 * 工具条不是「露出一半」而是整条被裁掉 —— 那片区域看着是空的，点下去命中的是
 * 视口外面的东西（贴上沿时是流程条）。对话坞写出来的第一张剧本卡就自动落在
 * 画布顶部，正好落进这个情形，用户不先把画布往下拖就一个按钮都点不到。
 *
 * 上方放不下时钳到卡片里去，不翻到下沿：卡片高度由正文撑，card.h 只是排版用的
 * 名义值（剧本卡一律 200，实际从几十到两百多都有），拿它算下沿会把工具条丢在
 * 卡片中间或者悬空的地方。
 */
export function toolbarPosition(
  card: CanvasCard,
  viewport: Viewport,
  toolbar: Size,
  viewportSize: Size,
): { left: number; top: number } {
  const right = (card.x + card.w) * viewport.k + viewport.x;
  const above = card.y * viewport.k + viewport.y - toolbar.h;
  return {
    left: clamp(right - toolbar.w, EDGE, viewportSize.w - toolbar.w - EDGE),
    top: clamp(above, EDGE, viewportSize.h - toolbar.h - EDGE),
  };
}
