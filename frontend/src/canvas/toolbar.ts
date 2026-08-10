import type { CanvasCard } from '@/api/types';
import type { Size } from './useElementSize';
import type { Viewport } from './useViewport';

/** 工具条与视口四边之间至少留这么多 */
const EDGE = 8;
/** 浮层与视口四边之间留这么多，跟 --space-4 对齐 */
const PANEL_EDGE = 16;
/** 浮层与工具条之间的间隙，够看出是两块东西就行 */
const GAP = 8;

/** 视口坐标系里的矩形，左上角 + 尺寸 */
export interface Rect extends Size {
  left: number;
  top: number;
}

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

function intersects(a: Rect, b: Rect): boolean {
  return a.left < b.left + b.w && b.left < a.left + a.w && a.top < b.top + b.h && b.top < a.top + a.h;
}

function withinViewport(r: Rect, viewportSize: Size): boolean {
  return (
    r.left >= PANEL_EDGE &&
    r.top >= PANEL_EDGE &&
    r.left + r.w <= viewportSize.w - PANEL_EDGE &&
    r.top + r.h <= viewportSize.h - PANEL_EDGE
  );
}

/**
 * 「优化剧本」面板在视口坐标系里的落点：老位置能用就不动，压住工具条才让开。
 *
 * 面板盖在工具条上（z 1000 对 200）不只是好看不好看的问题：那 5 个按钮全都
 * 命中不到，用户点开面板之后想顺手看眼版本历史就点不动了。工具条自己钳在
 * 视口里（见 toolbarPosition），落点随卡片跑，所以谁躲谁只能是面板躲工具条。
 *
 * 按候选顺序挑第一个既装得下又不相交的：老落点 → 工具条下方 → 上方 → 右侧 →
 * 左侧。优先竖着让，因为工具条通常贴在卡片上沿附近、是条矮横条，下移一点点
 * 就错开了；横着让要跨过 420 的面板宽，视觉跳动大得多。全都装不下（视口比
 * 面板还小）时退回老落点：那种尺寸下怎么摆都会压，不值得为它再加一档。
 */
export function refinePanelPosition(panel: Size, toolbar: Rect, viewportSize: Size): { left: number; top: number } {
  const home = { left: PANEL_EDGE, top: PANEL_EDGE };
  const candidates = [
    home,
    { left: PANEL_EDGE, top: toolbar.top + toolbar.h + GAP },
    { left: PANEL_EDGE, top: toolbar.top - panel.h - GAP },
    { left: toolbar.left + toolbar.w + GAP, top: PANEL_EDGE },
    { left: toolbar.left - panel.w - GAP, top: PANEL_EDGE },
  ];
  const fit = candidates.find((c) => {
    const rect = { ...c, w: panel.w, h: panel.h };
    return withinViewport(rect, viewportSize) && !intersects(rect, toolbar);
  });
  return fit ?? home;
}
