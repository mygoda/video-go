import type { CanvasCard, CharacterParams } from '@/api/types';
import type { JsonValue } from '@/schema/types';

/** 角色卡的排布尺寸。比镜头卡矮：它只有一张定妆图和几行外观，没有台词那一段 */
export const CHARACTER_CARD_W = 240;
export const CHARACTER_CARD_H = 220;
export const CHARACTER_GAP = 32;

/**
 * 第 index 张角色卡的落点：剧本卡右侧一列竖排。
 *
 * 放右边而不是下面：下方那片是镜头卡的格子（shotSlot），角色卡挤进去会被
 * 重排推来推去，而角色不属于任何一镜。
 */
export function characterSlot(script: CanvasCard, index: number): { x: number; y: number } {
  return {
    x: Math.round(script.x + script.w + CHARACTER_GAP),
    y: Math.round(script.y + index * (CHARACTER_CARD_H + CHARACTER_GAP)),
  };
}

function asText(value: JsonValue | undefined): string {
  return typeof value === 'string' ? value : '';
}

/**
 * 读出一张角色卡的结构化外观。
 *
 * 缺字段兜底而不是报错：角色卡允许先建出来再一项项填，空的 params 是合法状态。
 */
export function readCharacter(card: CanvasCard): CharacterParams {
  const params = card.params ?? {};
  return {
    name: asText(params.name),
    age: asText(params.age),
    build: asText(params.build),
    hair: asText(params.hair),
    outfit: asText(params.outfit),
    extra: asText(params.extra),
  };
}

/**
 * 写回 params 时必须是**完整的**全部字段，不能只带改动的那几个：
 * card.update 的 params 是整列覆盖，漏掉的字段会被抹成空。
 * 多带一个后端不认识的键则整批 op 被 400 顶回来。
 */
export function characterParams(c: CharacterParams): Record<string, JsonValue> {
  return {
    name: c.name,
    age: c.age,
    build: c.build,
    hair: c.hair,
    outfit: c.outfit,
    extra: c.extra,
  };
}

/**
 * 把结构化外观拼成一句 prompt 片段，逐项带上中文标签。
 *
 * ## 为什么带标签，而不是把几个值逗号连起来
 *
 * 这一段会跟镜头描述一起进 prompt。不带标签的话「黑色高马尾，米色风衣，
 * 雨夜的巷口」在模型看来是一串并列的画面元素，风衣有可能被画成挂在墙上的
 * 一件衣服；带上「发型：」「衣着：」它才是在描述一个人。
 *
 * ## 为什么空项直接跳过
 *
 * 角色卡是一项项填起来的。空项照拼会得到「衣着：」这种半句，模型只能猜，
 * 而它猜出来的东西每一镜都不一样 —— 那正是这张卡要修的问题。
 *
 * 全空返回空串，调用方据此整段不拼。
 */
export function characterLook(c: CharacterParams): string {
  const parts = [
    ['年龄', c.age],
    ['体貌', c.build],
    ['发型', c.hair],
    ['衣着', c.outfit],
    ['其他', c.extra],
  ]
    .filter(([, value]) => value.trim() !== '')
    .map(([label, value]) => `${label}：${value.trim()}`);
  if (!parts.length) return '';
  const name = c.name.trim();
  return `${name ? `角色「${name}」` : '角色'}（${parts.join('，')}）`;
}

/**
 * 定妆图的 prompt。
 *
 * 固定加上「全身正面站姿、纯色背景」：定妆图的用处是给用户一个"这个人长这样"
 * 的锚点，也可能被当成某一镜的首帧图喂给出片模型。带场景的话那个场景会跟着
 * 进成片，而用户要的只是这个人。
 *
 * **不加"写实""照片"这类词**：上游内容审核会以 `input image 'content[1]' may
 * contain real person` 拒收写实人像（T11 实测撞到过）。画风由角色卡的 extra
 * 字段说了算，用户填「二维赛璐璐插画风」这类词才走得通 —— 这条约束写在
 * 编辑器的提示里，不在这里硬塞，否则用户填的画风会被我们悄悄改掉。
 */
export function characterLookPrompt(c: CharacterParams): string {
  const look = characterLook(c);
  if (!look) return '';
  return `${look} 的角色定妆图，全身正面站姿，纯色背景，无文字`;
}

/** 这张角色卡有没有定妆图。定妆图就是这张卡自己的产物，不另占 params 字段 */
export function hasLookSheet(card: CanvasCard): boolean {
  return (card.asset_id ?? '') !== '';
}

/** 属于某张剧本卡的角色卡，按建卡先后排，保证顺序稳定 */
export function charactersOf(cards: CanvasCard[], scriptId: string): CanvasCard[] {
  return cards
    .filter((c) => c.kind === 'character' && c.refs.includes(scriptId))
    .sort((a, b) => a.created_at.localeCompare(b.created_at));
}

/**
 * 一张镜头卡上选中的那几个角色，按 character_ids 里的顺序。
 *
 * 查不到的 id 直接跳过：角色卡可能已经被删了，而镜头卡的 params 里还留着
 * 那个 id。为此报错没有意义 —— 用户能做的也只是把它取消掉。
 */
export function shotCharacters(cards: CanvasCard[], characterIDs: string[]): CanvasCard[] {
  return characterIDs
    .map((id) => cards.find((c) => c.id === id && c.kind === 'character'))
    .filter((c): c is CanvasCard => c !== undefined);
}
