import type { CanvasCard } from '@/api/types';
import { cardTitle } from './cardTitle';
import { shotsOf } from './shot';
import { charactersOf } from './character';

/** 框离成员卡的留白（世界坐标像素）。 */
const PAD = 26;

interface Box {
  minX: number;
  minY: number;
  maxX: number;
  maxY: number;
}

function boxOf(cards: CanvasCard[]): Box | null {
  if (!cards.length) return null;
  return cards.reduce<Box>(
    (a, c) => ({
      minX: Math.min(a.minX, c.x),
      minY: Math.min(a.minY, c.y),
      maxX: Math.max(a.maxX, c.x + c.w),
      maxY: Math.max(a.maxY, c.y + c.h),
    }),
    { minX: Infinity, minY: Infinity, maxX: -Infinity, maxY: -Infinity },
  );
}

interface LineGroupsProps {
  /** 应用了拖拽实时位置后的卡片，框才跟着拖动的卡走。 */
  cards: CanvasCard[];
  /** 当前创作线的剧本 id（activeScript），它的框高亮。 */
  activeScriptId: string | null;
}

/**
 * 创作线分组层：给每一条「剧本 + 它的分镜 + 角色（+ 成片卡）」套一个淡框 + 标题，
 * 再从剧本画一条主连线到分镜簇。挂在 .canvas-world 的最前面（cards.map 之前），
 * 与卡片共用世界坐标、且画在卡片之下；整层 pointer-events:none，不挡卡片拖拽。
 *
 * 只圈「有分镜或角色」的线：一张孤零零的剧本卡没有可归拢的东西，不画框。
 */
export function LineGroups({ cards, activeScriptId }: LineGroupsProps) {
  const groups = cards
    .filter((c) => c.kind === 'script')
    .map((script) => {
      const shots = shotsOf(cards, script.id);
      const chars = charactersOf(cards, script.id);
      // 成片卡（阶段 2）也 refs 回剧本，届时会自然进 members——这里按 kind 收全。
      const kin = cards.filter(
        (c) => (c.kind === 'video' || c.kind === 'image') && c.refs.includes(script.id),
      );
      const members = [script, ...shots, ...chars, ...kin];
      if (members.length <= 1) return null;
      return {
        script,
        box: boxOf(members)!,
        shotBox: boxOf(shots),
        active: script.id === activeScriptId,
      };
    })
    .filter((g): g is NonNullable<typeof g> => g !== null);

  if (!groups.length) return null;

  return (
    <div className="line-groups" aria-hidden="true">
      {groups.map(({ script, box, active }) => (
        <div
          key={script.id}
          className={`line-group${active ? ' active' : ''}`}
          style={{
            left: box.minX - PAD,
            top: box.minY - PAD,
            width: box.maxX - box.minX + PAD * 2,
            height: box.maxY - box.minY + PAD * 2,
          }}
        >
          <span className="line-group-title">🎬 创作线 · {cardTitle(script)}</span>
        </div>
      ))}
      {/* 主连线：剧本底边中点 → 分镜簇顶边中点。overflow:visible 让线画到 svg 视口外的世界坐标。 */}
      <svg className="line-links" width={1} height={1} style={{ position: 'absolute', left: 0, top: 0, overflow: 'visible' }}>
        {groups.map(({ script, shotBox }) => {
          if (!shotBox) return null;
          const x1 = script.x + script.w / 2;
          const y1 = script.y + script.h;
          const x2 = (shotBox.minX + shotBox.maxX) / 2;
          const y2 = shotBox.minY;
          const mid = (y1 + y2) / 2;
          return (
            <path
              key={script.id}
              className="line-link"
              d={`M ${x1} ${y1} C ${x1} ${mid} ${x2} ${mid} ${x2} ${y2}`}
              fill="none"
            />
          );
        })}
      </svg>
    </div>
  );
}
