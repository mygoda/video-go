import type { CanvasCard } from '@/api/types';
import type { JsonValue } from '@/schema/types';

/**
 * 剧本正文的一个历史版本，即某次保存**之前**的内容
 * （形状见后端 domain.ScriptVersion）。
 */
export type ScriptVersion = {
  /** 版号，从 1 起，全局递增；超出上限丢掉最旧几版之后它不等于数组下标 */
  rev: number;
  title: string;
  text: string;
  /** RFC3339，这一版被替换掉的时刻 */
  at: string;
};

/**
 * 剧本卡的版本列表在 `params.versions`，**由服务端维护**：每次 patch 正文，
 * 后端把改动前的标题与正文追加成一版（store/mysql 的 scriptVersionSet）。
 * 前端只读不写 —— 直接 patch script 卡的 params 会被 400 顶回来，版本历史
 * 是只增不改的日志，能改就不再是证据。
 *
 * 因此保存之后必须重新拉一次画布才看得到新版本，见 CanvasPage.patchScript。
 */
export function readVersions(card: CanvasCard): ScriptVersion[] {
  const raw = card.params?.versions;
  if (!Array.isArray(raw)) return [];
  const out: ScriptVersion[] = [];
  raw.forEach((item, index) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) return;
    const v = item as Record<string, JsonValue>;
    if (typeof v.text !== 'string') return;
    out.push({
      rev: typeof v.rev === 'number' ? v.rev : index + 1,
      title: typeof v.title === 'string' ? v.title : '',
      text: v.text,
      at: typeof v.at === 'string' ? v.at : '',
    });
  });
  return out;
}
