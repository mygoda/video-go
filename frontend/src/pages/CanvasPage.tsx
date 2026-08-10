import { useCallback, useMemo, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import type { CanvasCard, CanvasOp, CanvasState, ShotParams } from '@/api/types';
import { qk, useCanvas, useMe, useProjects } from '@/api/queries';
import { useAuthStore } from '@/stores/auth';
import { useViewport } from '@/canvas/useViewport';
import { useCanvasSync } from '@/canvas/useCanvasSync';
import { CanvasCardView } from '@/canvas/CanvasCardView';
import { ConversationDock } from '@/canvas/ConversationDock';
import { CardRerunPanel } from '@/canvas/CardRerunPanel';
import { ComposeBar } from '@/canvas/ComposeBar';
import { ScriptVersionsPanel } from '@/canvas/ScriptVersionsPanel';
import type { ScriptVersion } from '@/canvas/script';
import { readShot, relayoutShots, shotParams, shotSlot, shotsOf, SHOT_CARD_H, SHOT_CARD_W } from '@/canvas/shot';

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
  const { enqueue, flush, saveState } = useCanvasSync(projectId);

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [rerunOpen, setRerunOpen] = useState(false);
  // 就地编辑同一时刻只有一张卡：两张卡同时开着编辑器，用户改完一张去点另一张
  // 时会以为两边都保存了，实际上只提交了后点的那张。
  const [editingId, setEditingId] = useState<string | null>(null);
  const [versionsOpen, setVersionsOpen] = useState(false);
  const [droppedRefs, setDroppedRefs] = useState<string[]>([]);
  // 合成模式：点选卡片不再是"选中"，而是往片段序列里追加。数组顺序即成片顺序，
  // 所以它是 string[] 而不是 Set——用户点选的先后是这个功能唯一的排序依据。
  const [composing, setComposing] = useState(false);
  const [picks, setPicks] = useState<string[]>([]);
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
      const ops: CanvasOp[] = [{ type: 'card.move', id: drag.cardId, x, y }];
      // 用户手动挪过的卡片退出自动重排（后端 storyboardCards 就是这么约定的）。
      // 少了这一条，改一次镜号就会把用户特意摆开的那张镜头卡拽回格子里。
      const moved = cards.find((c) => c.id === drag.cardId);
      if (moved?.auto_placed) ops.push({ type: 'card.update', id: drag.cardId, patch: { auto_placed: false } });
      enqueue(ops, (prev) => ({
        ...prev,
        cards: prev.cards.map((c) => (c.id === drag.cardId ? { ...c, x, y, auto_placed: false } : c)),
      }));
    },
    [cards, dragPos, enqueue],
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
      // 不带 title：标题由 cardTitle() 按 kind 兜底，用户没起过名的卡不该被前端
      // 先斩后奏地写死一个名字（后端补上标题后会随卡片带回来）。
      text: '双击编辑…',
      refs: [],
      history: [],
      auto_placed: false,
      created_at: new Date().toISOString(),
    };
    enqueue([{ type: 'card.create', card }], (prev) => ({ ...prev, cards: [...prev.cards, card] }));
    setSelectedId(card.id);
  }

  function deleteSelected(): void {
    if (!selected) return;
    const id = selected.id;
    setSelectedId(null);
    setEditingId((cur) => (cur === id ? null : cur));
    enqueue([{ type: 'card.delete', id }], (prev) => ({ ...prev, cards: prev.cards.filter((c) => c.id !== id) }));
  }

  /** 剧本卡：正文空着建出来，直接进编辑态，用户不用再找哪儿能写。 */
  function addScriptCard(): void {
    const rect = viewportEl?.getBoundingClientRect();
    const at = rect ? toWorld(rect.left + rect.width / 2, rect.top + 160) : { x: 0, y: 0 };
    const card: CanvasCard = {
      id: `c_${Date.now().toString(36)}`,
      kind: 'script',
      x: Math.round(at.x),
      y: Math.round(at.y),
      w: 320,
      h: 200,
      z: cards.length + 1,
      text: '',
      refs: [],
      history: [],
      auto_placed: false,
      created_at: new Date().toISOString(),
    };
    enqueue([{ type: 'card.create', card }], (prev) => ({ ...prev, cards: [...prev.cards, card] }));
    setSelectedId(card.id);
    setEditingId(card.id);
  }

  /**
   * 镜头卡挂在当前选中的剧本卡下：refs 指回剧本（血缘就是这么记的，不建边表），
   * 镜号顺着已有的最大镜号加一，落点直接取该镜号的格子。
   */
  function addShotCard(): void {
    if (!selected || selected.kind !== 'script') return;
    const script = selected;
    const siblings = shotsOf(cards, script.id);
    const nextNo = siblings.reduce((max, s) => Math.max(max, readShot(s).shot_no), 0) + 1;
    const at = shotSlot(script, siblings.length);
    const card: CanvasCard = {
      id: `c_${Date.now().toString(36)}`,
      kind: 'shot',
      x: at.x,
      y: at.y,
      w: SHOT_CARD_W,
      h: SHOT_CARD_H,
      z: cards.length + 1,
      params: shotParams({
        shot_no: nextNo,
        description: '',
        dialogue: '',
        duration_sec: 0,
        camera: '',
        shot_size: '',
      }),
      refs: [script.id],
      history: [],
      auto_placed: true,
      created_at: new Date().toISOString(),
    };
    enqueue([{ type: 'card.create', card }], (prev) => ({ ...prev, cards: [...prev.cards, card] }));
    setSelectedId(card.id);
    setEditingId(card.id);
  }

  /**
   * 剧本正文的保存只发 text（必要时带上回退的标题）。版本是服务端在这次 patch
   * 里自己追加的，前端多发一个 params 就会被 400 顶回来（store/mysql 的
   * errScriptParamsNotPatchable）。也正因为版本在服务端生成，写完必须立刻把
   * 队列冲掉再重取画布，否则版本历史里看不到刚存的这一版。
   */
  async function patchScript(card: CanvasCard, patch: Partial<CanvasCard>): Promise<void> {
    enqueue([{ type: 'card.update', id: card.id, patch }], (prev) => ({
      ...prev,
      cards: prev.cards.map((c) => (c.id === card.id ? { ...c, ...patch } : c)),
    }));
    await flush();
    await qc.invalidateQueries({ queryKey: qk.canvas(projectId) });
  }

  function saveScript(card: CanvasCard, text: string): void {
    setEditingId(null);
    if ((card.text ?? '') === text) return;
    void patchScript(card, { text });
  }

  /** 回退：把那一版的正文（和它当时的标题，如果有）写回当前，这次回退本身也会被服务端留成一版。 */
  function restoreScript(card: CanvasCard, version: ScriptVersion): void {
    if ((card.text ?? '') === version.text) return;
    const patch: Partial<CanvasCard> = { text: version.text };
    if (version.title && version.title !== card.title) patch.title = version.title;
    void patchScript(card, patch);
  }

  /** 改完镜头就按镜号重排一次：镜号是分镜的顺序，画布上的位置必须跟着它走。 */
  function saveShot(card: CanvasCard, shot: ShotParams): void {
    setEditingId(null);
    const params = shotParams(shot);
    const next = cards.map((c) => (c.id === card.id ? { ...c, params } : c));
    const moves = card.refs.length ? relayoutShots(next, card.refs[0]) : [];
    const ops: CanvasOp[] = [
      { type: 'card.update', id: card.id, patch: { params } },
      ...moves.map((m) => ({ type: 'card.move' as const, id: m.id, x: m.x, y: m.y })),
    ];
    enqueue(ops, (prev) => ({
      ...prev,
      cards: prev.cards.map((c) => {
        const base = c.id === card.id ? { ...c, params } : c;
        const move = moves.find((m) => m.id === c.id);
        return move ? { ...base, x: move.x, y: move.y } : base;
      }),
    }));
  }

  // 合成模式下点卡片是加/减片段；再点一次同一张就把它取消，序号自动前移。
  function onCardClick(id: string): void {
    if (!composing) {
      setSelectedId(id);
      return;
    }
    setPicks((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
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
          <button type="button" className="btn btn-sm" onClick={addScriptCard}>
            ＋ 剧本
          </button>
          <button
            type="button"
            className="btn btn-sm"
            disabled={selected?.kind !== 'script'}
            title={selected?.kind === 'script' ? '在这张剧本下加一个镜头' : '先选中一张剧本卡'}
            onClick={addShotCard}
          >
            ＋ 镜头
          </button>
          <button
            type="button"
            className={`btn btn-sm${composing ? ' btn-primary' : ''}`}
            onClick={() => {
              setComposing((on) => !on);
              setSelectedId(null);
              setPicks([]);
            }}
          >
            🎬 合成
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
                pickIndex={picks.indexOf(card.id) + 1}
                editing={card.id === editingId}
                onSelect={onCardClick}
                onDragStart={onCardDragStart}
                onEditStart={setEditingId}
                onEditCancel={() => setEditingId(null)}
                onSaveScript={saveScript}
                onSaveShot={saveShot}
              />
            );
          })}
        </div>

        <div className="canvas-overlay">
          {selected && (
            <div className="card-toolbar" style={{ left: toolbarLeft, top: toolbarTop }}>
              {(selected.kind === 'script' || selected.kind === 'shot') && (
                <button
                  type="button"
                  className="icon-btn"
                  title="编辑正文（也可以双击卡片）"
                  aria-label="编辑正文"
                  onClick={() => setEditingId(selected.id)}
                >
                  ✎
                </button>
              )}
              {selected.kind === 'script' && (
                <button
                  type="button"
                  className="icon-btn"
                  title="版本历史"
                  aria-label="版本历史"
                  aria-pressed={versionsOpen}
                  onClick={() => setVersionsOpen((on) => !on)}
                >
                  ⟲
                </button>
              )}
              <button
                type="button"
                className="icon-btn"
                title="重跑（片段重拍）"
                aria-label="重跑"
                // 重跑要拿模型重出一件产物，只有图片 / 视频卡有这回事；
                // 剧本和镜头的正文是用户写的，改它走就地编辑。
                disabled={selected.kind !== 'image' && selected.kind !== 'video'}
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

          {versionsOpen && selected?.kind === 'script' && (
            <ScriptVersionsPanel
              card={selected}
              onRestore={(version) => restoreScript(selected, version)}
              onClose={() => setVersionsOpen(false)}
            />
          )}

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

          {/* 合成条与对话坞都占底部，同时出现会互相压住；合成是个短暂的模式，
              退出就回到对话。 */}
          {composing ? (
            <ComposeBar
              projectId={projectId}
              picks={picks}
              cards={cards}
              onClear={() => setPicks([])}
              onExit={() => {
                setComposing(false);
                setPicks([]);
              }}
            />
          ) : (
            <ConversationDock
              projectId={projectId}
              conversation={canvas.conversation}
              refCards={refCards}
              onRemoveRef={(id) => setDroppedRefs((prev) => [...prev, id])}
            />
          )}
        </div>
      </div>
    </div>
  );
}
