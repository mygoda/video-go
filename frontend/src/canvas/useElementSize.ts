import { useLayoutEffect, useState } from 'react';

export interface Size {
  w: number;
  h: number;
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
