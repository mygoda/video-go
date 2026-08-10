import type { CanvasCard } from '@/api/types';

/**
 * 一张剧本卡下最多几个镜头。与后端 storyboard.go 的 storyboardMaxShots 对齐。
 *
 * 后端越界会 400，但界面必须在用户点下去之前就把话说清楚：让人填了 20、点了
 * 「继续」才被顶回来，和根本没有上限一样难用。
 */
export const MAX_SHOTS = 12;

/** 不改就拆几个镜头。与后端 storyboardDefaultShots 对齐 */
export const DEFAULT_SHOTS = 3;

export type FlowStepId = 'idea' | 'script' | 'storyboard' | 'first_frame' | 'render' | 'compose';

/** 已完成 / 当前 / 未解锁。没有第四态：能动的永远只有「当前」这一步 */
export type FlowStepState = 'done' | 'current' | 'locked';

export interface FlowStep {
  id: FlowStepId;
  label: string;
  state: FlowStepState;
}

const STEPS: { id: FlowStepId; label: string }[] = [
  { id: 'idea', label: '一句话灵感' },
  { id: 'script', label: '剧本' },
  { id: 'storyboard', label: '分镜' },
  { id: 'first_frame', label: '首帧' },
  { id: 'render', label: '出片' },
  { id: 'compose', label: '合成' },
];

/**
 * 流程条盯着的那张剧本卡。
 *
 * 一张画布上可以有好几条创作线（用户再说一句话就又多一份剧本），所以「走到
 * 哪一步」这个问题得先回答「哪一条线」。选中的卡片就是答案：选中镜头卡时顺
 * refs 回到它的剧本。什么都没选就取最新的那份——那是用户刚产出、多半正盯着的。
 */
export function activeScript(cards: CanvasCard[], selectedId: string | null): CanvasCard | null {
  const selected = cards.find((c) => c.id === selectedId);
  if (selected?.kind === 'script') return selected;
  if (selected?.kind === 'shot') {
    const parent = cards.find((c) => c.kind === 'script' && selected.refs.includes(c.id));
    if (parent) return parent;
  }
  const scripts = cards.filter((c) => c.kind === 'script');
  if (!scripts.length) return null;
  return scripts.reduce((latest, c) => (c.created_at > latest.created_at ? c : latest));
}

export function currentStep(script: CanvasCard | null, shotCount: number): FlowStepId {
  if (!script) return 'idea';
  return shotCount === 0 ? 'script' : 'storyboard';
}

/**
 * 整条链路每一步的状态。
 *
 * 后三步（首帧 / 出片 / 合成）恒定未解锁：那几层还没放行。它们在条上占位是
 * 为了让用户看得见这条路还有多长，不是为了让他点。
 */
export function flowSteps(script: CanvasCard | null, shotCount: number): FlowStep[] {
  const currentIndex = STEPS.findIndex((s) => s.id === currentStep(script, shotCount));
  return STEPS.map((step, i): FlowStep => ({
    ...step,
    state: i < currentIndex ? 'done' : i === currentIndex ? 'current' : 'locked',
  }));
}

/**
 * 镜头数不合法时的说法。返回一句能直接摆到界面上的话而不是 boolean——
 * 「不能点」和「为什么不能」必须一起给出来。
 */
export function shotCountError(count: number): string | null {
  if (!Number.isInteger(count) || count < 1) return '镜头数至少 1';
  if (count > MAX_SHOTS) return `镜头数最多 ${MAX_SHOTS}；镜头越多，下一步出片越贵`;
  return null;
}
