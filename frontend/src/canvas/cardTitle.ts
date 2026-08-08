import type { CanvasCard, CardKind } from '@/api/types';

const KIND_LABEL: Record<CardKind, string> = {
  text: '文本',
  image: '图片',
  video: '视频',
};

/**
 * 卡片标题的唯一来源。后端 Card 没有 title 列，新建时带上的标题会被丢弃，
 * 刷新后就成了 undefined —— 所以渲染层一律按 kind 兜底。后端补上 title 之后
 * 这里自动优先用它，不需要再改各个渲染点。
 */
export function cardTitle(card: CanvasCard): string {
  return card.title?.trim() || KIND_LABEL[card.kind];
}
