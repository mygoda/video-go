import { useEffect, useRef, useState } from 'react';

interface NewProjectDialogProps {
  /** 创建在飞。失败时弹框不关，用户改完名字能直接重试 */
  busy: boolean;
  onCancel(): void;
  onSubmit(name: string): void;
}

export function NewProjectDialog({ busy, onCancel, onSubmit }: NewProjectDialogProps) {
  const [name, setName] = useState('新的短剧');
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    inputRef.current?.select();
  }, []);

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent): void {
      if (e.key === 'Escape' && !busy) onCancel();
    }
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [busy, onCancel]);

  const trimmed = name.trim();

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
        aria-label="新建项目"
        onSubmit={(e) => {
          e.preventDefault();
          if (!trimmed || busy) return;
          onSubmit(trimmed);
        }}
      >
        <h2>新建项目</h2>
        <div>
          <label className="sr-only" htmlFor="new-project-name">
            项目名称
          </label>
          <input
            id="new-project-name"
            ref={inputRef}
            className="input"
            value={name}
            autoFocus
            disabled={busy}
            placeholder="项目名称"
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div className="dialog-foot">
          {!trimmed && <span className="hint">填个名字才能创建</span>}
          <button type="button" className="btn" disabled={busy} onClick={onCancel}>
            取消
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !trimmed}>
            {busy ? '创建中…' : '创建'}
          </button>
        </div>
      </form>
    </div>
  );
}
