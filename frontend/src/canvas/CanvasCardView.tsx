import { useQuery } from '@tanstack/react-query';
import type { CanvasCard, ShotParams, Task } from '@/api/types';
import { assetPreview } from '@/api/types';
import { api } from '@/api/endpoints';
import { useTasks } from '@/api/queries';
import { AssetTextBody } from '@/components/AssetTextBody';
import { displayProgress } from '@/components/task/failure';
import { cardTitle, KIND_ICON } from './cardTitle';
import { ScriptCardBody } from './ScriptCardBody';
import { ShotCardBody } from './ShotCardBody';

interface CanvasCardViewProps {
  card: CanvasCard;
  k: number;
  visible: boolean;
  selected: boolean;
  lineage: boolean;
  dimmed: boolean;
  /** 合成模式下的片段序号（从 1 起）；未入选为 0。序号即成片里的先后 */
  pickIndex: number;
  /** 就地编辑中。同一时刻只有一张卡处于这个状态，由画布页统一持有 */
  editing: boolean;
  onSelect(id: string): void;
  onDragStart(e: React.PointerEvent, card: CanvasCard): void;
  onEditStart(id: string): void;
  onEditCancel(): void;
  onSaveScript(card: CanvasCard, text: string): void;
  onSaveShot(card: CanvasCard, shot: ShotParams): void;
}

/**
 * 分级渲染（frontend-design.md §5.5）：缩到很小就别加载图，
 * 一块渐变色和一张 512 缩略图在 k=0.3 下看起来没区别，但内存差一个量级。
 */
function useAsset(assetId: string | undefined, wanted: boolean) {
  return useQuery({
    queryKey: ['asset', assetId],
    queryFn: () => api.asset(assetId!),
    enabled: Boolean(assetId) && wanted,
    staleTime: Infinity,
  });
}

function runningTask(tasks: Task[] | undefined, taskId: string | undefined): Task | undefined {
  if (!taskId) return undefined;
  const task = tasks?.find((t) => t.id === taskId);
  return task && (task.status === 'queued' || task.status === 'running') ? task : undefined;
}

export function CanvasCardView({
  card,
  k,
  visible,
  selected,
  lineage,
  dimmed,
  pickIndex,
  editing,
  onSelect,
  onDragStart,
  onEditStart,
  onEditCancel,
  onSaveScript,
  onSaveShot,
}: CanvasCardViewProps) {
  const wantsMedia = k >= 0.4 && visible;
  const { data: asset } = useAsset(card.asset_id, wantsMedia);
  const { data: tasks } = useTasks();
  const pending = runningTask(tasks, card.task_id);
  // text 产物没有可预览的画面，assetPreview 会给 null；卡片 kind 是建卡时定的，
  // 挡不住一张 image 卡上挂着一件 text 资产，所以判据取资产本身。
  const preview = asset ? assetPreview(asset) : null;
  // 剧本与镜头的正文是用户自己写的文字，就地编辑；其余卡片的内容来自产物，
  // 改它没有意义（要换内容走重跑）。
  const editable = card.kind === 'script' || card.kind === 'shot';

  const className = [
    'canvas-card',
    // 这里的 visual / text 是**卡片宽度**，与 kind 不是一一对应：
    // 剧本和镜头都按文本宽度排版。
    card.kind === 'image' || card.kind === 'video' ? 'visual' : 'text',
    selected ? 'selected' : '',
    lineage ? 'lineage' : '',
    dimmed ? 'dimmed' : '',
    pickIndex > 0 ? 'picked' : '',
    editing ? 'editing' : '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <article
      className={className}
      style={{ left: card.x, top: card.y, width: card.w, zIndex: editing ? card.z + 1000 : card.z }}
      role="group"
      tabIndex={0}
      aria-label={cardTitle(card)}
      aria-selected={selected}
      onPointerDown={(e) => {
        onSelect(card.id);
        onDragStart(e, card);
      }}
      onDoubleClick={() => {
        if (editable && !editing) onEditStart(card.id);
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onSelect(card.id);
        }
      }}
    >
      <div className="cap">
        {KIND_ICON[card.kind]} {cardTitle(card)}
      </div>

      {pickIndex > 0 && <span className="pick-index mono">{pickIndex}</span>}

      {/* 剧本与镜头排在媒体分支之前，且不读 asset：正文是文字，落到下面任何一条
          <img>/<video> 分支都是一张裂图（DEM-78 已经为 text 产物修过一次）。 */}
      {card.kind === 'script' ? (
        <ScriptCardBody
          card={card}
          editing={editing}
          onSave={(text) => onSaveScript(card, text)}
          onCancel={onEditCancel}
        />
      ) : card.kind === 'shot' ? (
        <ShotCardBody
          card={card}
          editing={editing}
          onSave={(shot) => onSaveShot(card, shot)}
          onCancel={onEditCancel}
        />
      ) : card.kind === 'text' ? (
        <div className="body">{card.text}</div>
      ) : pending ? (
        // 生成中：原位骨架屏，位置尺寸不动，避免画布跳版
        <div className="media" style={{ position: 'relative' }}>
          <div className="skeleton" />
          <div className="task-state" style={{ background: 'transparent' }}>
            <div className="progress">
              <i style={{ width: `${Math.round(displayProgress(pending) * 100)}%` }} />
            </div>
            <div className="sub mono">{pending.eta_seconds ? `${pending.eta_seconds}s` : '生成中'}</div>
          </div>
        </div>
      ) : !wantsMedia || !asset ? (
        <div className="art media" />
      ) : !preview ? (
        <AssetTextBody asset={asset} className="media" />
      ) : card.kind === 'video' ? (
        // <video> 很贵：只在放得够大且在视口里才真的挂上去
        k >= 0.6 ? (
          <video className="media" src={asset.original} poster={asset.poster ?? undefined} controls playsInline preload="none" />
        ) : (
          <img className="media" src={preview} alt={cardTitle(card)} style={{ objectFit: 'cover' }} />
        )
      ) : (
        <img
          className="media"
          src={k >= 1 ? asset.original : preview}
          alt={card.prompt ?? cardTitle(card)}
          style={{ objectFit: 'cover' }}
        />
      )}

      {(card.history?.length ?? 0) > 1 && (
        <span className="version-switch">
          <span className="mono">
            {card.history?.length}/{card.history?.length}
          </span>
        </span>
      )}
    </article>
  );
}
