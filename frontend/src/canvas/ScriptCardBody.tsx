import { useState } from 'react';
import type { CanvasCard } from '@/api/types';

interface ScriptCardBodyProps {
  card: CanvasCard;
  editing: boolean;
  onSave(text: string): void;
  onCancel(): void;
}

/**
 * 剧本卡的正文。
 *
 * 正文是纯文本，永远走 `<div>`，绝不进 `<img src>` —— text 产物那一次裂图
 * （DEM-78）就是这么来的：把一份 text/plain 的 URL 喂给了图片元素。
 */
export function ScriptCardBody({ card, editing, onSave, onCancel }: ScriptCardBodyProps) {
  if (editing) {
    return <ScriptEditor card={card} onSave={onSave} onCancel={onCancel} />;
  }
  const text = (card.text ?? '').trim();
  return <div className="body card-text">{text || '（还没有正文，双击编辑）'}</div>;
}

function ScriptEditor({ card, onSave, onCancel }: Omit<ScriptCardBodyProps, 'editing'>) {
  const [draft, setDraft] = useState(card.text ?? '');
  const fieldId = `script-text-${card.id}`;

  return (
    <div
      className="card-editor"
      // 编辑区在卡片内部，而卡片的 pointerdown 会开始拖动并 setPointerCapture ——
      // 指针一旦被卡片捕获，输入框就再也收不到 pointerup，点不动也选不中文字。
      onPointerDown={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        // 卡片本身把 Enter / 空格当作"选中"，不拦住的话正文里敲不了空格
        e.stopPropagation();
        if (e.key === 'Escape') onCancel();
        if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) onSave(draft);
      }}
    >
      <label className="sr-only" htmlFor={fieldId}>
        剧本正文
      </label>
      <textarea
        id={fieldId}
        className="textarea"
        style={{ height: 200 }}
        value={draft}
        autoFocus
        onChange={(e) => setDraft(e.target.value)}
      />
      <div className="card-editor-actions">
        <span className="hint">⌘/Ctrl+Enter 保存</span>
        <button type="button" className="btn btn-sm" onClick={onCancel}>
          取消
        </button>
        <button type="button" className="btn btn-sm btn-primary" onClick={() => onSave(draft)}>
          保存
        </button>
      </div>
    </div>
  );
}
