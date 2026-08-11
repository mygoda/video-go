import { useEffect, useRef, type ReactNode } from 'react';

interface ConfirmDialogProps {
  title: string;
  /** 正文，说清后果；不可逆的要自己写明「不可撤销」 */
  body: ReactNode;
  confirmLabel: string;
  /** 处理中的按钮文案，默认「处理中…」 */
  busyLabel?: string;
  /** 不可逆操作：确认键走危险色 */
  danger?: boolean;
  /** mutation 在飞。失败时弹框不关，用户能看到 toast 直接重试 */
  busy?: boolean;
  onCancel(): void;
  onConfirm(): void;
}

export function ConfirmDialog({
  title,
  body,
  confirmLabel,
  busyLabel = '处理中…',
  danger = false,
  busy = false,
  onCancel,
  onConfirm,
}: ConfirmDialogProps) {
  const confirmRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    confirmRef.current?.focus();
  }, []);

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent): void {
      if (e.key === 'Escape' && !busy) onCancel();
    }
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [busy, onCancel]);

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && !busy) onCancel();
      }}
    >
      <form
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onSubmit={(e) => {
          e.preventDefault();
          if (busy) return;
          onConfirm();
        }}
      >
        <h2>{title}</h2>
        <div className="dialog-body">{body}</div>
        <div className="dialog-foot">
          <button type="button" className="btn" disabled={busy} onClick={onCancel}>
            取消
          </button>
          <button
            ref={confirmRef}
            type="submit"
            className={danger ? 'btn btn-danger' : 'btn btn-primary'}
            disabled={busy}
          >
            {busy ? busyLabel : confirmLabel}
          </button>
        </div>
      </form>
    </div>
  );
}
