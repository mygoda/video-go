import { useQuery } from '@tanstack/react-query';
import type { CanvasCard, Task } from '@/api/types';
import { assetPreview } from '@/api/types';
import { api } from '@/api/endpoints';
import { useTasks } from '@/api/queries';
import { displayProgress } from '@/components/task/failure';
import { cardTitle } from './cardTitle';

interface CanvasCardViewProps {
  card: CanvasCard;
  k: number;
  visible: boolean;
  selected: boolean;
  lineage: boolean;
  dimmed: boolean;
  onSelect(id: string): void;
  onDragStart(e: React.PointerEvent, card: CanvasCard): void;
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
  onSelect,
  onDragStart,
}: CanvasCardViewProps) {
  const wantsMedia = k >= 0.4 && visible;
  const { data: asset } = useAsset(card.asset_id, wantsMedia);
  const { data: tasks } = useTasks();
  const pending = runningTask(tasks, card.task_id);

  const className = [
    'canvas-card',
    card.kind === 'text' ? 'text' : 'shot',
    selected ? 'selected' : '',
    lineage ? 'lineage' : '',
    dimmed ? 'dimmed' : '',
  ]
    .filter(Boolean)
    .join(' ');

  const icon = card.kind === 'text' ? '📝' : card.kind === 'video' ? '🎬' : '🖼';

  return (
    <article
      className={className}
      style={{ left: card.x, top: card.y, width: card.w, zIndex: card.z }}
      role="group"
      tabIndex={0}
      aria-label={cardTitle(card)}
      aria-selected={selected}
      onPointerDown={(e) => {
        onSelect(card.id);
        onDragStart(e, card);
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          onSelect(card.id);
        }
      }}
    >
      <div className="cap">
        {icon} {cardTitle(card)}
      </div>

      {card.kind === 'text' ? (
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
      ) : card.kind === 'video' ? (
        // <video> 很贵：只在放得够大且在视口里才真的挂上去
        k >= 0.6 ? (
          <video className="media" src={asset.original} poster={asset.poster ?? undefined} controls playsInline preload="none" />
        ) : (
          <img className="media" src={assetPreview(asset)} alt={cardTitle(card)} style={{ objectFit: 'cover' }} />
        )
      ) : (
        <img
          className="media"
          src={k >= 1 ? asset.original : assetPreview(asset)}
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
