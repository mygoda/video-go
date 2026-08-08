import { useEffect, useRef, type ReactNode } from 'react';

interface SheetProps {
  title: string;
  onClose(): void;
  children: ReactNode;
  footer: ReactNode;
}

/**
 * 右侧抽屉。管理端的表单（尤其 capability schema）比弹窗高得多，
 * 抽屉能一路往下滚而不遮住列表的上下文。
 */
export function Sheet({ title, onClose, children, footer }: SheetProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent): void {
      if (e.key === 'Escape') onClose();
    }
    document.addEventListener('keydown', onKeyDown);
    panelRef.current?.focus();
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [onClose]);

  return (
    <div
      className="sheet-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        className="sheet"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        ref={panelRef}
      >
        <div className="sheet-head">
          <h2>{title}</h2>
          <span className="spacer" />
          <button type="button" className="btn btn-sm btn-ghost" onClick={onClose}>
            关闭
          </button>
        </div>
        {children}
        <div className="sheet-foot">{footer}</div>
      </div>
    </div>
  );
}
