import { useState } from 'react';
import type { CanvasCard, ShotParams } from '@/api/types';
import { readShot } from './shot';

interface ShotCardBodyProps {
  card: CanvasCard;
  editing: boolean;
  onSave(shot: ShotParams): void;
  onCancel(): void;
}

/**
 * 镜头卡的正文：镜号在卡片标题里，这里是画面描述与台词。
 *
 * 台词单独占一块而不是和描述连排：它是出片时唯一会被念出来的部分，
 * 混在描述里用户就看不出这一镜到底有没有人说话。
 */
export function ShotCardBody({ card, editing, onSave, onCancel }: ShotCardBodyProps) {
  const shot = readShot(card);
  if (editing) {
    return <ShotEditor card={card} shot={shot} onSave={onSave} onCancel={onCancel} />;
  }

  const meta = [shot.shot_size, shot.camera, shot.duration_sec > 0 ? `${shot.duration_sec}s` : '']
    .filter(Boolean)
    .join(' · ');

  return (
    <div className="body card-text">
      {meta && <div className="shot-meta mono">{meta}</div>}
      <p className="shot-desc">{shot.description || '（还没有镜头描述，双击编辑）'}</p>
      {shot.dialogue && <p className="shot-dialogue">「{shot.dialogue}」</p>}
    </div>
  );
}

function ShotEditor({ card, shot, onSave, onCancel }: Omit<ShotCardBodyProps, 'editing'> & { shot: ShotParams }) {
  const [draft, setDraft] = useState(shot);
  const id = (field: string) => `shot-${field}-${card.id}`;

  function set<K extends keyof ShotParams>(key: K, value: ShotParams[K]): void {
    setDraft((prev) => ({ ...prev, [key]: value }));
  }

  return (
    <div
      className="card-editor"
      onPointerDown={(e) => e.stopPropagation()}
      onDoubleClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        e.stopPropagation();
        if (e.key === 'Escape') onCancel();
        if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) onSave(draft);
      }}
    >
      <div className="card-editor-row">
        <label htmlFor={id('no')}>镜号</label>
        <input
          id={id('no')}
          className="num-input"
          type="number"
          min={1}
          step={1}
          value={draft.shot_no}
          autoFocus
          onChange={(e) => set('shot_no', Math.max(1, Math.round(Number(e.target.value) || 1)))}
        />
        <label htmlFor={id('dur')}>时长</label>
        <input
          id={id('dur')}
          className="num-input"
          type="number"
          min={0}
          step={1}
          value={draft.duration_sec}
          onChange={(e) => set('duration_sec', Math.max(0, Number(e.target.value) || 0))}
        />
      </div>

      <div className="card-editor-row">
        <label htmlFor={id('size')}>景别</label>
        <input
          id={id('size')}
          className="input"
          type="text"
          value={draft.shot_size}
          onChange={(e) => set('shot_size', e.target.value)}
        />
        <label htmlFor={id('cam')}>机位</label>
        <input
          id={id('cam')}
          className="input"
          type="text"
          value={draft.camera}
          onChange={(e) => set('camera', e.target.value)}
        />
      </div>

      <label className="card-editor-label" htmlFor={id('desc')}>
        镜头描述
      </label>
      <textarea
        id={id('desc')}
        className="textarea"
        style={{ minHeight: 64 }}
        value={draft.description}
        onChange={(e) => set('description', e.target.value)}
      />

      <label className="card-editor-label" htmlFor={id('line')}>
        台词
      </label>
      <textarea
        id={id('line')}
        className="textarea"
        style={{ minHeight: 48 }}
        value={draft.dialogue}
        onChange={(e) => set('dialogue', e.target.value)}
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
