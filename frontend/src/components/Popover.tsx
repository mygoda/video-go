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

  return (
    <div className="popover-anchor" ref={ref}>
      {trigger}
      {open && (
        <div
          className={`popover popover-float${alignRight ? ' align-right' : ''}${dropUp ? ' drop-up' : ''}`}
          role="dialog"
        >
          {children}
        </div>
      )}
    </div>
  );
}
