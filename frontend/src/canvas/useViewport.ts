import { useCallback, useEffect, useRef, useState } from 'react';

export interface Viewport {
  x: number;
  y: number;
  k: number;
}

const MIN_K = 0.15;
const MAX_K = 3;
const STORAGE_PREFIX = 'aigc_pool_viewport_';

function clampK(k: number): number {
  return Math.min(MAX_K, Math.max(MIN_K, k));
}

function load(projectId: string): Viewport {
  try {
    const raw = localStorage.getItem(STORAGE_PREFIX + projectId);
    if (raw) return JSON.parse(raw) as Viewport;
  } catch {
    // 存坏了就当没存过
  }
  return { x: 0, y: 0, k: 1 };
}

/**
 * 视口。平移/缩放期间只改这个 state，卡片本身不重排 ——
 * 整个世界是一个 transform 容器（frontend-design.md §5.2）。
 *
 * 收的是节点本身而不是 ref 对象：画布加载完成前会先渲染占位，
 * ref.current 在首次 effect 里还是 null，用 ref 的话 wheel 监听会永远挂不上。
 */
export function useViewport(projectId: string, node: HTMLElement | null) {
  const [viewport, setViewport] = useState<Viewport>(() => load(projectId));
  const [panning, setPanning] = useState(false);
  // 拖拽过程中的中间值放 ref：每帧 setState 会把 React 打爆
  const dragRef = useRef<{ pointerId: number; startX: number; startY: number; originX: number; originY: number } | null>(null);

  useEffect(() => {
    localStorage.setItem(STORAGE_PREFIX + projectId, JSON.stringify(viewport));
  }, [projectId, viewport]);

  /** 以光标为锚点缩放：鼠标底下的那个点必须原地不动，否则手感立刻散架 */
  const zoomAt = useCallback((clientX: number, clientY: number, factor: number) => {
    const rect = node?.getBoundingClientRect();
    if (!rect) return;
    setViewport((v) => {
      const k = clampK(v.k * factor);
      const scale = k / v.k;
      const px = clientX - rect.left;
      const py = clientY - rect.top;
      return { k, x: px - (px - v.x) * scale, y: py - (py - v.y) * scale };
    });
  }, [node]);

  const zoomBy = useCallback((factor: number) => {
    const rect = node?.getBoundingClientRect();
    if (!rect) return;
    zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, factor);
  }, [node, zoomAt]);

  const fit = useCallback((bounds: { minX: number; minY: number; maxX: number; maxY: number } | null) => {
    const rect = node?.getBoundingClientRect();
    if (!rect || !bounds) return;
    const pad = 80;
    const w = Math.max(bounds.maxX - bounds.minX, 1);
    const h = Math.max(bounds.maxY - bounds.minY, 1);
    // 只缩小以容纳内容，绝不放大：内容比视口小时就停在 100%。否则「适应」一张
    // 小卡片会把它怼到 2~3 倍，既晃眼又把周围别的卡片顶出视野。
    const k = clampK(Math.min(1, (rect.width - pad * 2) / w, (rect.height - pad * 2) / h));
    setViewport({
      k,
      x: rect.width / 2 - ((bounds.minX + bounds.maxX) / 2) * k,
      y: rect.height / 2 - ((bounds.minY + bounds.maxY) / 2) * k,
    });
  }, [node]);

  const onPointerDown = useCallback((e: React.PointerEvent) => {
    if (e.button !== 0) return;
    dragRef.current = { pointerId: e.pointerId, startX: e.clientX, startY: e.clientY, originX: viewport.x, originY: viewport.y };
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    setPanning(true);
  }, [viewport.x, viewport.y]);

  const onPointerMove = useCallback((e: React.PointerEvent) => {
    const drag = dragRef.current;
    if (!drag || drag.pointerId !== e.pointerId) return;
    setViewport((v) => ({ ...v, x: drag.originX + (e.clientX - drag.startX), y: drag.originY + (e.clientY - drag.startY) }));
  }, []);

  const onPointerUp = useCallback((e: React.PointerEvent) => {
    if (dragRef.current?.pointerId !== e.pointerId) return;
    dragRef.current = null;
    setPanning(false);
  }, []);

  // wheel 必须走非 passive 的原生监听，否则 preventDefault 被忽略、页面跟着滚
  useEffect(() => {
    if (!node) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      if (e.ctrlKey || e.metaKey) {
        zoomAt(e.clientX, e.clientY, Math.exp(-e.deltaY / 200));
      } else {
        setViewport((v) => ({ ...v, x: v.x - e.deltaX, y: v.y - e.deltaY }));
      }
    };
    node.addEventListener('wheel', onWheel, { passive: false });
    return () => node.removeEventListener('wheel', onWheel);
  }, [node, zoomAt]);

  const toWorld = useCallback((clientX: number, clientY: number) => {
    const rect = node?.getBoundingClientRect();
    if (!rect) return { x: 0, y: 0 };
    return { x: (clientX - rect.left - viewport.x) / viewport.k, y: (clientY - rect.top - viewport.y) / viewport.k };
  }, [node, viewport]);

  return { viewport, panning, zoomBy, fit, toWorld, handlers: { onPointerDown, onPointerMove, onPointerUp, onPointerCancel: onPointerUp } };
}
