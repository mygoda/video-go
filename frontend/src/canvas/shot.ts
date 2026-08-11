import type { CanvasCard, ShotParams } from '@/api/types';
import type { JsonValue } from '@/schema/types';

/**
 * 镜头卡的排布参数，沿用分镜那一步的 280×180 网格（httpapi/script.go 里
 * 估算画布高度时引用的就是这个尺寸）。后端在 T2 之后不再自己落镜头卡，
 * 格子只剩前端这一份，改这里等于改整块分镜区的版式。
 */
export const SHOT_CARD_W = 280;
export const SHOT_CARD_H = 180;
export const SHOT_GAP = 32;
export const SHOT_PER_ROW = 4;

function asText(value: JsonValue | undefined): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: JsonValue | undefined, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : fallback;
}

/**
 * 人声开关的三态：只有明确的 true / false 才算表过态，其余（缺键、null、
 * 存量卡里塞的别的东西）一律当作「还没人动过」，跟随全局默认。
 */
function asTriState(value: JsonValue | undefined): boolean | null {
  return typeof value === 'boolean' ? value : null;
}

/**
 * 出场角色的 id 列表。非数组、或数组里混进非字符串的项，一律当作没选 ——
 * 存量镜头卡的 params 里根本没有这个键，「缺席」是主路径不是边角。
 */
function asIdList(value: JsonValue | undefined): string[] {
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string') : [];
}

/**
 * 读出一张 shot 卡的结构化字段。
 *
 * 缺字段兜底而不是报错：镜头卡允许先建出来再填（分镜那一步会分批回写），
 * 空的 params 是合法状态。
 */
export function readShot(card: CanvasCard): ShotParams {
  const params = card.params ?? {};
  return {
    shot_no: Math.max(1, Math.round(asNumber(params.shot_no, 1))),
    description: asText(params.description),
    dialogue: asText(params.dialogue),
    voice: asTriState(params.voice),
    duration_sec: Math.max(0, asNumber(params.duration_sec, 0)),
    camera: asText(params.camera),
    shot_size: asText(params.shot_size),
    first_frame_asset_id: asText(params.first_frame_asset_id),
    character_ids: asIdList(params.character_ids),
  };
}

/**
 * 写回 params 时必须是**完整的**全部字段，不能只带改动的那几个：
 * card.update 的 params 是整列覆盖，漏掉的字段会被抹成空。
 * 多带一个后端不认识的键则整批 op 被 400 顶回来。
 */
export function shotParams(shot: ShotParams): Record<string, JsonValue> {
  return {
    shot_no: shot.shot_no,
    description: shot.description,
    dialogue: shot.dialogue,
    voice: shot.voice,
    duration_sec: shot.duration_sec,
    camera: shot.camera,
    shot_size: shot.shot_size,
    first_frame_asset_id: shot.first_frame_asset_id,
    character_ids: shot.character_ids,
  };
}

/** 属于某张剧本卡的镜头卡，按镜号升序；同镜号时按建卡先后，保证顺序稳定。 */
export function shotsOf(cards: CanvasCard[], scriptId: string): CanvasCard[] {
  return cards
    .filter((c) => c.kind === 'shot' && c.refs.includes(scriptId))
    .sort((a, b) => {
      const diff = readShot(a).shot_no - readShot(b).shot_no;
      return diff !== 0 ? diff : a.created_at.localeCompare(b.created_at);
    });
}

/** 第 index 张镜头卡（index 从 0 起）在剧本卡下方的格子坐标。 */
export function shotSlot(script: CanvasCard, index: number): { x: number; y: number } {
  const row = Math.floor(index / SHOT_PER_ROW);
  const col = index % SHOT_PER_ROW;
  return {
    x: Math.round(script.x + col * (SHOT_CARD_W + SHOT_GAP)),
    y: Math.round(script.y + script.h + SHOT_GAP * 2 + row * (SHOT_CARD_H + SHOT_GAP)),
  };
}

/**
 * 把一张剧本卡名下的镜头卡按镜号重新铺回格子里，返回需要挪动的卡片。
 *
 * 只动 auto_placed 的卡：用户手动拖开过的镜头卡（拖动时会把这个标记翻成
 * false）说明他要的就是那个位置，自动重排不该把它抢回来。这些卡照样占一个
 * 格子号，否则改一次镜号，画布上所有卡片都会跟着平移一格。
 */
export function relayoutShots(cards: CanvasCard[], scriptId: string): { id: string; x: number; y: number }[] {
  const script = cards.find((c) => c.id === scriptId && c.kind === 'script');
  if (!script) return [];
  const moves: { id: string; x: number; y: number }[] = [];
  shotsOf(cards, scriptId).forEach((shot, index) => {
    if (!shot.auto_placed) return;
    const at = shotSlot(script, index);
    if (shot.x !== at.x || shot.y !== at.y) moves.push({ id: shot.id, ...at });
  });
  return moves;
}
