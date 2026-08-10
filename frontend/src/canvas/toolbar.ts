import type { CanvasCard } from '@/api/types';
import type { Rect, Size } from './useElementSize';
import type { Viewport } from './useViewport';

/** 工具条与视口四边之间至少留这么多 */
const EDGE = 8;
/** 浮层与视口四边之间留这么多，跟 --space-4 对齐 */
const PANEL_EDGE = 16;
/** 两块浮层之间的间隙，够看出是两块东西就行 */
const GAP = 8;

/** 视口坐标系里的落点 */
interface Spot {
  left: number;
  top: number;
}

function clamp(value: number, min: number, max: number): number {
  // 视口比工具条还小时区间是空的，此时贴上/左，至少让人看得见它
  if (max < min) return min;
  return Math.min(max, Math.max(min, value));
}

function intersects(a: Rect, b: Rect): boolean {
  return a.left < b.left + b.w && b.left < a.left + a.w && a.top < b.top + b.h && b.top < a.top + a.h;
}

function withinViewport(r: Rect, viewportSize: Size, edge: number): boolean {
  return (
    r.left >= edge &&
    r.top >= edge &&
    r.left + r.w <= viewportSize.w - edge &&
    r.top + r.h <= viewportSize.h - edge
  );
}

/**
 * 按给定顺序挑第一个既装得进视口、又不压在任何 obstacle 上的落点。
 *
 * 画布上的浮层互相让路都归结成这一件事，区别只在候选怎么排。挑不出来时返回
 * undefined，由调用方决定退回哪儿——那种情形下怎么摆都会压，不该在这里编一个。
 */
function firstFit(
  candidates: readonly Spot[],
  size: Size,
  viewportSize: Size,
  edge: number,
  obstacles: readonly Rect[],
): Spot | undefined {
  return candidates.find((c) => {
    const rect = { ...c, ...size };
    return withinViewport(rect, viewportSize, edge) && !obstacles.some((o) => intersects(rect, o));
  });
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
 *
 * 光钳进视口还不够：视口里还浮着对话坞（z 300）和缩放条（同层但排在后面），
 * 两块都盖得住工具条，卡片拖到右下角时 5 个按钮一个都点不着。所以钳完还要绕开
 * overlays——它们是常驻主控件，不该为一条临时浮条挪窝，只能工具条自己让。
 * 先横着绕再竖着绕：坞贴着右下角、比工具条高得多，横着挪一次就出来了，竖着挪
 * 得跨过整个坞，工具条会甩到离卡片很远的地方。
 */
export function toolbarPosition(
  card: CanvasCard,
  viewport: Viewport,
  toolbar: Size,
  viewportSize: Size,
  overlays: readonly Rect[],
): Spot {
  const right = (card.x + card.w) * viewport.k + viewport.x;
  const above = card.y * viewport.k + viewport.y - toolbar.h;
  const home = {
    left: clamp(right - toolbar.w, EDGE, viewportSize.w - toolbar.w - EDGE),
    top: clamp(above, EDGE, viewportSize.h - toolbar.h - EDGE),
  };
  const candidates = [
    home,
    ...overlays.flatMap((o) => [
      { left: o.left - toolbar.w - GAP, top: home.top },
      { left: o.left + o.w + GAP, top: home.top },
      { left: home.left, top: o.top - toolbar.h - GAP },
      { left: home.left, top: o.top + o.h + GAP },
    ]),
  ];
  return firstFit(candidates, toolbar, viewportSize, EDGE, overlays) ?? home;
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
export function refinePanelPosition(panel: Size, toolbar: Rect, viewportSize: Size): Spot {
  const home = { left: PANEL_EDGE, top: PANEL_EDGE };
  const candidates = [
    home,
    { left: PANEL_EDGE, top: toolbar.top + toolbar.h + GAP },
    { left: PANEL_EDGE, top: toolbar.top - panel.h - GAP },
    { left: toolbar.left + toolbar.w + GAP, top: PANEL_EDGE },
    { left: toolbar.left - panel.w - GAP, top: PANEL_EDGE },
  ];
  return firstFit(candidates, panel, viewportSize, PANEL_EDGE, [toolbar]) ?? home;
}
