import type { CanvasCard, CardKind } from '@/api/types';
import { readShot } from './shot';

const KIND_LABEL: Record<CardKind, string> = {
  text: '文本',
  image: '图片',
  video: '视频',
  script: '剧本',
  shot: '镜头',
};

export const KIND_ICON: Record<CardKind, string> = {
  text: '📝',
  image: '🖼',
  video: '🎬',
  script: '📜',
  shot: '🎞',
};

/**
 * 卡片标题的唯一来源。后端 Card 没有 title 列，新建时带上的标题会被丢弃，
 * 刷新后就成了 undefined —— 所以渲染层一律按 kind 兜底。后端补上 title 之后
 * 这里自动优先用它，不需要再改各个渲染点。
 *
 * 镜头卡兜底成「镜 N」而不是「镜头」：一屏十几张镜头卡全叫同一个名字，
 * 用户就没法在画布上认出这是第几镜。
 */
export function cardTitle(card: CanvasCard): string {
  const explicit = card.title?.trim();
  if (explicit) return explicit;
  if (card.kind === 'shot') return `镜 ${readShot(card).shot_no}`;
  return KIND_LABEL[card.kind];
}
