import { useLayoutEffect, useState } from 'react';

export interface Size {
  w: number;
  h: number;
}

/** 某个坐标系里的矩形，左上角 + 尺寸 */
export interface Rect extends Size {
  left: number;
  top: number;
}

/**
 * 节点的边框盒尺寸，节点自身或窗口变化时重测。
 *
 * 收节点本身而不是 ref 对象，理由同 useViewport：工具条是条件渲染的，
 * ref.current 在首次 effect 里还是 null，量到的尺寸会永远停在 0。
 * 用 layout effect 而不是 effect：量不到就得先按 0 画一次，那一帧的位置
 * 是错的，用户看得见它跳。
 */
export function useElementSize(node: HTMLElement | null): Size {
  const [size, setSize] = useState<Size>({ w: 0, h: 0 });

  useLayoutEffect(() => {
    if (!node) return;
    const measure = (): void => {
      const rect = node.getBoundingClientRect();
      setSize((prev) => (prev.w === rect.width && prev.h === rect.height ? prev : { w: rect.width, h: rect.height }));
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    return () => observer.disconnect();
  }, [node]);

  return size;
}

/**
 * 节点在 container 坐标系里的矩形，任一方尺寸变化时重测。
 *
 * 比 useElementSize 多一份位置，两个节点都要盯着：常驻浮层（对话坞、缩放条）
 * 贴着视口的边角摆，视口一改尺寸它们的位置就变，自身尺寸却一动不动——只观察
 * 浮层自己的话，窗口缩放后拿到的还是旧位置。
 *
 * 节点没挂上时返回 null 而不是一个零矩形：零矩形会被当成一块真挡路的东西
 * 参与避让计算，让工具条为一块不存在的浮层挪窝。
 */
export function useElementRect(node: HTMLElement | null, container: HTMLElement | null): Rect | null {
  const [rect, setRect] = useState<Rect | null>(null);

  useLayoutEffect(() => {
    if (!node || !container) {
      setRect(null);
      return;
    }
    const measure = (): void => {
      const at = node.getBoundingClientRect();
      const base = container.getBoundingClientRect();
      const next: Rect = { left: at.left - base.left, top: at.top - base.top, w: at.width, h: at.height };
      setRect((prev) =>
        prev && prev.left === next.left && prev.top === next.top && prev.w === next.w && prev.h === next.h ? prev : next,
      );
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    observer.observe(container);
    return () => observer.disconnect();
  }, [node, container]);

  return rect;
}
