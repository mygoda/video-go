import { useCallback, useMemo, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasCard, CanvasState } from '@/api/types';
import { qk, useCanvas, useMe, useProjects } from '@/api/queries';
import { useAuthStore } from '@/stores/auth';
import { useViewport } from '@/canvas/useViewport';
import { useCanvasSync } from '@/canvas/useCanvasSync';
import { CanvasCardView } from '@/canvas/CanvasCardView';
import { ConversationDock } from '@/canvas/ConversationDock';
import { CardRerunPanel } from '@/canvas/CardRerunPanel';

interface DragState {
  pointerId: number;
  cardId: string;
  startX: number;
  startY: number;
  originX: number;
  originY: number;
}

function bounds(cards: CanvasCard[]): { minX: number; minY: number; maxX: number; maxY: number } | null {
  if (!cards.length) return null;
  return cards.reduce(
    (acc, c) => ({
      minX: Math.min(acc.minX, c.x),
      minY: Math.min(acc.minY, c.y),
      maxX: Math.max(acc.maxX, c.x + c.w),
      maxY: Math.max(acc.maxY, c.y + c.h),
    }),
    { minX: Infinity, minY: Infinity, maxX: -Infinity, maxY: -Infinity },
  );
}

export function CanvasPage() {
  const { projectId = '' } = useParams<{ projectId: string }>();
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const { data: me } = useMe(isAuthed);
  const { data: projects } = useProjects();
  const { data: canvas, isLoading } = useCanvas(projectId);
  const qc = useQueryClient();

  const [viewportEl, setViewportEl] = useState<HTMLDivElement | null>(null);
  const { viewport, panning, zoomBy, fit, toWorld, handlers } = useViewport(projectId, viewportEl);
  const { enqueue, saveState } = useCanvasSync(projectId);

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [rerunOpen, setRerunOpen] = useState(false);
  const [droppedRefs, setDroppedRefs] = useState<string[]>([]);
  // 拖卡片的中间位置只落在这里，松手才写进缓存并排队上行
  const dragRef = useRef<DragState | null>(null);
  const [dragPos, setDragPos] = useState<{ id: string; x: number; y: number } | null>(null);

  const project = projects?.find((p) => p.id === projectId);
  const cards = canvas?.cards ?? [];
  const selected = cards.find((c) => c.id === selectedId) ?? null;
  const lineageIds = new Set(selected?.refs ?? []);

  const refCards = useMemo(
    () => (selected && !droppedRefs.includes(selected.id) ? [selected] : []),
    [selected, droppedRefs],
  );

  const onCardDragStart = useCallback((e: React.PointerEvent, card: CanvasCard) => {
    e.stopPropagation();
    dragRef.current = {
      pointerId: e.pointerId,
      cardId: card.id,
      startX: e.clientX,
      startY: e.clientY,
      originX: card.x,
      originY: card.y,
    };
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
  }, []);

  const onCardDragMove = useCallback(
    (e: React.PointerEvent) => {
      const drag = dragRef.current;
      if (!drag || drag.pointerId !== e.pointerId) return;
      e.stopPropagation();
      setDragPos({
        id: drag.cardId,
        x: drag.originX + (e.clientX - drag.startX) / viewport.k,
        y: drag.originY + (e.clientY - drag.startY) / viewport.k,
      });
    },
    [viewport.k],
  );

  const onCardDragEnd = useCallback(
    (e: React.PointerEvent) => {
      const drag = dragRef.current;
      if (!drag || drag.pointerId !== e.pointerId) return;
      e.stopPropagation();
      dragRef.current = null;
      const pos = dragPos;
      setDragPos(null);
      if (!pos) return;
      const x = Math.round(pos.x);
      const y = Math.round(pos.y);
      enqueue([{ type: 'card.move', id: drag.cardId, x, y }], (prev) => ({
        ...prev,
        cards: prev.cards.map((c) => (c.id === drag.cardId ? { ...c, x, y } : c)),
      }));
    },
    [dragPos, enqueue],
  );

  function addTextCard(): void {
    const rect = viewportEl?.getBoundingClientRect();
    const at = rect ? toWorld(rect.left + rect.width / 2, rect.top + 160) : { x: 0, y: 0 };
    const card: CanvasCard = {
      id: `c_${Date.now().toString(36)}`,
      kind: 'text',
      x: Math.round(at.x),
      y: Math.round(at.y),
      w: 220,
      h: 120,
      z: cards.length + 1,
      title: '便签',
      text: '双击编辑…',
      refs: [],
      history: [],
      auto_placed: false,
      created_at: Date.now(),
    };
    enqueue([{ type: 'card.create', card }], (prev) => ({ ...prev, cards: [...prev.cards, card] }));
    setSelectedId(card.id);
  }

  function deleteSelected(): void {
    if (!selected) return;
    const id = selected.id;
    setSelectedId(null);
    enqueue([{ type: 'card.delete', id }], (prev) => ({ ...prev, cards: prev.cards.filter((c) => c.id !== id) }));
  }

  if (isLoading || !canvas) {
    return <div className="empty">画布加载中…</div>;
  }

  const toolbarLeft = selected ? selected.x * viewport.k + viewport.x + selected.w * viewport.k - 96 : 0;
  const toolbarTop = selected ? selected.y * viewport.k + viewport.y - 38 : 0;

  return (
    <div className="canvas-screen">
      {/* 画布用更窄的顶栏：这里视口比导航值钱 */}
      <header className="canvas-topbar">
        <Link className="btn btn-sm btn-ghost" to="/projects">
          ← 返回
        </Link>
        <span className="proj-name">{project?.name ?? '画布'}</span>
        <span className="rev mono">
          {saveState === 'pending' ? '保存中…' : saveState === 'error' ? '已重新同步' : '已保存'} · rev {canvas.revision}
        </span>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8, alignItems: 'center' }}>
          <span className="credit-badge">
            ⚡ <span className="mono">{(me?.credits ?? 0).toLocaleString()}</span>
          </span>
          <button type="button" className="btn btn-sm" onClick={addTextCard}>
            ＋ 便签
          </button>
          <button type="button" className="btn btn-sm" disabled={!selected} onClick={deleteSelected}>
            删除卡片
          </button>
        </div>
      </header>

      <div
        ref={setViewportEl}
        className={`canvas-viewport${panning ? ' panning' : ''}`}
        style={{ backgroundSize: `${24 * viewport.k}px ${24 * viewport.k}px`, backgroundPosition: `${viewport.x}px ${viewport.y}px` }}
        onPointerDown={(e) => {
          // 只有落在空白处才算平移。落在卡片或浮层控件上时绝不能 setPointerCapture ——
          // 指针一旦被视口捕获，按钮就收不到 pointerup，click 永远不会派发。
          const onBackground =
            e.target === e.currentTarget || (e.target as HTMLElement).classList.contains('canvas-world');
          if (!onBackground) return;
          setSelectedId(null);
          handlers.onPointerDown(e);
        }}
        onPointerMove={(e) => {
          onCardDragMove(e);
          if (!dragRef.current) handlers.onPointerMove(e);
        }}
        onPointerUp={(e) => {
          onCardDragEnd(e);
          handlers.onPointerUp(e);
        }}
        onPointerCancel={handlers.onPointerCancel}
      >
        {/* 所有卡片放进这一个 transform 容器，平移缩放只重排这一层 */}
        <div
          className="canvas-world"
          style={{ transform: `translate(${viewport.x}px, ${viewport.y}px) scale(${viewport.k})` }}
        >
          {cards.map((card) => {
            const live = dragPos?.id === card.id ? { ...card, x: dragPos.x, y: dragPos.y } : card;
            return (
              <CanvasCardView
                key={card.id}
                card={live}
                k={viewport.k}
                visible
                selected={card.id === selectedId}
                lineage={lineageIds.has(card.id)}
                dimmed={Boolean(selectedId) && card.id !== selectedId && !lineageIds.has(card.id)}
                onSelect={setSelectedId}
                onDragStart={onCardDragStart}
              />
            );
          })}
        </div>

        <div className="canvas-overlay">
          {selected && (
            <div className="card-toolbar" style={{ left: toolbarLeft, top: toolbarTop }}>
              <button
                type="button"
                className="icon-btn"
                title="重跑（片段重拍）"
                aria-label="重跑"
                disabled={selected.kind === 'text'}
                onClick={() => setRerunOpen(true)}
              >
                ↻
              </button>
              <button
                type="button"
                className="icon-btn"
                title="引用为下次输入"
                aria-label="引用为下次输入"
                onClick={() => setDroppedRefs((prev) => prev.filter((id) => id !== selected.id))}
              >
                ⊹
              </button>
            </div>
          )}

          <div className="viewport-controls">
            <button type="button" className="vc-btn" aria-label="缩小" onClick={() => zoomBy(1 / 1.2)}>
              ⊖
            </button>
            <span className="mono" style={{ minWidth: 38, textAlign: 'center' }}>
              {Math.round(viewport.k * 100)}%
            </span>
            <button type="button" className="vc-btn" aria-label="放大" onClick={() => zoomBy(1.2)}>
              ⊕
            </button>
            <span style={{ width: 1, height: 16, background: 'var(--border-subtle)' }} />
            <button type="button" className="vc-btn" aria-label="适应全部内容" onClick={() => fit(bounds(cards))}>
              ⛶
            </button>
          </div>

          {rerunOpen && selected && (
            <CardRerunPanel
              projectId={projectId}
              card={selected}
              onClose={() => setRerunOpen(false)}
              onSubmitted={(taskId) => {
                // 重跑期间这张卡原位显示骨架屏，靠 task_id 关联
                qc.setQueryData<CanvasState>(qk.canvas(projectId), (prev) =>
                  prev
                    ? { ...prev, cards: prev.cards.map((c) => (c.id === selected.id ? { ...c, task_id: taskId } : c)) }
                    : prev,
                );
              }}
            />
          )}

          <ConversationDock
            projectId={projectId}
            conversation={canvas.conversation}
            refCards={refCards}
            onRemoveRef={(id) => setDroppedRefs((prev) => [...prev, id])}
          />
        </div>
      </div>
    </div>
  );
}
