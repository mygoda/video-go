import type { CanvasCard } from '@/api/types';
import { cardTitle } from './cardTitle';
import { shotsOf } from './shot';
import { charactersOf } from './character';

/** 框离成员卡的留白（世界坐标像素）。 */
const PAD = 26;

const STORAGE_PREFIX = 'aigc_pool_lines_collapsed_';

/** 读某画布折叠了哪些创作线（按剧本 id）。 */
export function loadCollapsed(projectId: string): Set<string> {
  try {
    const raw = localStorage.getItem(STORAGE_PREFIX + projectId);
    return new Set<string>(raw ? (JSON.parse(raw) as string[]) : []);
  } catch {
    return new Set();
  }
}

/** 存折叠状态。 */
export function saveCollapsed(projectId: string, ids: ReadonlySet<string>): void {
  try {
    localStorage.setItem(STORAGE_PREFIX + projectId, JSON.stringify([...ids]));
  } catch {
    /* localStorage 满 / 无痕模式：折叠状态丢了不影响功能 */
  }
}

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
  /** 折叠了的创作线（按剧本 id）。 */
  collapsed: ReadonlySet<string>;
  /** 折叠 / 展开一条线。 */
  onToggle(scriptId: string): void;
  /** 整组重跑一条线（每镜重出成片 + 重合成）。 */
  onRerun(scriptId: string): void;
}

/**
 * 创作线分组层：给每一条「剧本 + 它的分镜 + 角色 + 成片卡」套一个淡框 + 标题，
 * 再从剧本画一条主连线到分镜簇。挂在 .canvas-world 的最前面（cards.map 之前），
 * 与卡片共用世界坐标、且画在卡片之下；整层 pointer-events:none，只有标题条上的
 * 折叠按钮单独接管指针。折叠时框收成一颗药丸（成员卡由 CanvasPage 隐藏）。
 *
 * 只圈「有分镜或角色」的线：一张孤零零的剧本卡没有可归拢的东西，不画框。
 */
export function LineGroups({ cards, activeScriptId, collapsed, onToggle, onRerun }: LineGroupsProps) {
  const groups = cards
    .filter((c) => c.kind === 'script')
    .map((script) => {
      const shots = shotsOf(cards, script.id);
      const chars = charactersOf(cards, script.id);
      const kin = cards.filter(
        (c) => (c.kind === 'video' || c.kind === 'image') && c.refs.includes(script.id),
      );
      const members = [script, ...shots, ...chars, ...kin];
      if (members.length <= 1) return null;
      return {
        script,
        box: boxOf(members)!,
        shotBox: boxOf(shots),
        shotCount: shots.length,
        hasCut: kin.some((c) => c.kind === 'video' && c.asset_id),
        active: script.id === activeScriptId,
        isCollapsed: collapsed.has(script.id),
      };
    })
    .filter((g): g is NonNullable<typeof g> => g !== null);

  if (!groups.length) return null;

  return (
    <div className="line-groups" aria-hidden="false">
      {groups.map((g) => {
        const { script, box, active, isCollapsed, shotCount, hasCut } = g;
        if (isCollapsed) {
          // 收起态：一颗药丸落在剧本原位，成员卡已被隐藏。
          return (
            <div
              key={script.id}
              className={`line-pill${active ? ' active' : ''}`}
              style={{ left: script.x, top: script.y }}
            >
              <button
                type="button"
                className="line-toggle"
                title="展开创作线"
                onPointerDown={(e) => e.stopPropagation()}
                onClick={() => onToggle(script.id)}
              >
                ▸
              </button>
              🎬 {cardTitle(script)}
              <span className="line-pill-sub">
                {shotCount} 镜{hasCut ? ' · 已出成片' : ''}
              </span>
            </div>
          );
        }
        return (
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
            <span className="line-group-title">
              <button
                type="button"
                className="line-toggle"
                title="收起创作线"
                onPointerDown={(e) => e.stopPropagation()}
                onClick={() => onToggle(script.id)}
              >
                ▾
              </button>
              🎬 创作线 · {cardTitle(script)}
              {g.shotCount > 0 && (
                <button
                  type="button"
                  className="line-rerun"
                  title="整组重跑：每镜重出成片，再重合成"
                  onPointerDown={(e) => e.stopPropagation()}
                  onClick={() => onRerun(script.id)}
                >
                  ⟳ 重跑
                </button>
              )}
            </span>
          </div>
        );
      })}
      {/* 主连线：剧本底边中点 → 分镜簇顶边中点；折叠的线不画。 */}
      <svg className="line-links" width={1} height={1} style={{ position: 'absolute', left: 0, top: 0, overflow: 'visible' }}>
        {groups.map(({ script, shotBox, isCollapsed }) => {
          if (isCollapsed || !shotBox) return null;
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
