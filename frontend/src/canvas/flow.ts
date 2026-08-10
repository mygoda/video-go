import type { CanvasCard } from '@/api/types';
import { hasFirstFrame } from './firstFrame';
import { hasVideo } from './render';

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

/**
 * 这条线走到哪一步了。
 *
 * 首帧那一步的判据是**镜头卡有没有首帧产物**，不是"点过没点过按钮"：
 * 按钮点过但出图失败、或者用户点完就加了一镜，这条线实际上还没出完首帧。
 * 只有每一镜都有图，这一步才算走到。出片同理，判据是镜头卡上有没有视频产物
 * （`card.asset_id`，首帧不占这个字段）。
 *
 * 到 render 为止。合成恒定未解锁：那一步由用户在顶栏点「🎬 合成」自己进，
 * 出片走完不会把他推进去 —— 每一镜都得先看过才知道要不要重出。
 */
export function currentStep(script: CanvasCard | null, shots: CanvasCard[]): FlowStepId {
  if (!script) return 'idea';
  if (shots.length === 0) return 'script';
  if (!shots.every(hasFirstFrame)) return 'storyboard';
  return shots.every(hasVideo) ? 'render' : 'first_frame';
}

/**
 * 整条链路每一步的状态。
 *
 * 合成恒定未解锁：它在条上占位是为了让用户看得见这条路还有多长，
 * 而进那一步的入口是顶栏的「🎬 合成」，不是这里。
 */
export function flowSteps(script: CanvasCard | null, shots: CanvasCard[]): FlowStep[] {
  const currentIndex = STEPS.findIndex((s) => s.id === currentStep(script, shots));
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
