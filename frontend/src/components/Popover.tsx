import { useEffect, useRef, type ReactNode } from 'react';

interface PopoverProps {
  open: boolean;
  onClose(): void;
  trigger: ReactNode;
  alignRight?: boolean;
  /** 往上开。锚点贴着屏幕底边时（对话坞的芯片行）往下开会整个掉到视口外面 */
  dropUp?: boolean;
  children: ReactNode;
}

/**
 * 芯片弹层。故意不做 portal + floating 计算：芯片栏在 composer 内部，
 * 绝对定位到锚点下方就够了，少一层依赖也少一处 z-index 打架。
 * 方向由调用方按自己所处的位置指定，不做自动翻转 —— 那就得测量，又绕回浮动计算。
 */
export function Popover({ open, onClose, trigger, alignRight, dropUp, children }: PopoverProps) {
  const ref = useRef<HTMLDivElement>(null);
  const floatRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open, onClose]);

  // 弹层挂在画布里，而画布在祖先元素上用原生 wheel 监听平移/缩放。滚弹层里的
  // 长列表时，wheel 会冒泡到那个监听上，结果画布跟着动、列表纹丝不动。React 的
  // onWheel 拦不住（祖先的原生监听在冒泡阶段先跑），必须用原生监听在弹层这一层
  // 就 stopPropagation。只挡传播、不 preventDefault，列表照常原生滚动。
  useEffect(() => {
    const el = floatRef.current;
    if (!open || !el) return;
    const stop = (e: WheelEvent) => e.stopPropagation();
    el.addEventListener('wheel', stop);
    return () => el.removeEventListener('wheel', stop);
  }, [open]);

  return (
    <div className="popover-anchor" ref={ref}>
      {trigger}
      {open && (
        <div
          ref={floatRef}
          className={`popover popover-float${alignRight ? ' align-right' : ''}${dropUp ? ' drop-up' : ''}`}
          role="dialog"
        >
          {children}
        </div>
      )}
    </div>
  );
}
