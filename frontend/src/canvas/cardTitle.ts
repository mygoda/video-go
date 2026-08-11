import type { CanvasCard, CardKind } from '@/api/types';
import { readCharacter } from './character';
import { readShot } from './shot';

const KIND_LABEL: Record<CardKind, string> = {
  text: '文本',
  image: '图片',
  video: '视频',
  script: '剧本',
  shot: '镜头',
  character: '角色',
};

export const KIND_ICON: Record<CardKind, string> = {
  text: '📝',
  image: '🖼',
  video: '🎬',
  script: '📜',
  shot: '🎞',
  character: '🧑',
};

/**
 * 卡片标题的唯一来源。后端 Card 没有 title 列，新建时带上的标题会被丢弃，
 * 刷新后就成了 undefined —— 所以渲染层一律按 kind 兜底。后端补上 title 之后
 * 这里自动优先用它，不需要再改各个渲染点。
 *
 * 镜头卡兜底成「镜 N」而不是「镜头」：一屏十几张镜头卡全叫同一个名字，
 * 用户就没法在画布上认出这是第几镜。角色卡同理兜底成角色名 —— 镜头编辑器里
 * 勾选角色时读的就是这个标题，几张卡全叫「角色」就没法选。
 */
export function cardTitle(card: CanvasCard): string {
  // 镜头卡永远带上镜号：拆分镜给的标题是场景名（「隧道入口对峙」），光看名字
  // 认不出这是第几镜。镜号 + 场景名一起显示，画布上一眼就知道顺序。
  if (card.kind === 'shot') {
    const no = readShot(card).shot_no;
    const explicit = card.title?.trim();
    return explicit ? `镜${no} · ${explicit}` : `镜 ${no}`;
  }
  const explicit = card.title?.trim();
  if (explicit) return explicit;
  if (card.kind === 'character') return readCharacter(card).name.trim() || KIND_LABEL[card.kind];
  return KIND_LABEL[card.kind];
}
