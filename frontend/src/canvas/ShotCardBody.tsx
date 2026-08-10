import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { CanvasCard, ShotParams } from '@/api/types';
import { assetPreview } from '@/api/types';
import { api } from '@/api/endpoints';
import { readShot } from './shot';

interface ShotCardBodyProps {
  card: CanvasCard;
  /** 这一镜的首帧正在出。批量与单张重出都会把它翻成 true */
  firstFramePending: boolean;
}

/**
 * 镜头卡的正文：镜号在卡片标题里，这里是首帧图、画面描述与台词。
 *
 * 台词单独占一块而不是和描述连排：它是出片时唯一会被念出来的部分，
 * 混在描述里用户就看不出这一镜到底有没有人说话。
 *
 * 首帧图排在最上面：出完图这一步要停下来等用户逐张确认，而「确认」就是看图，
 * 图得比描述先进入视线。
 */
export function ShotCardBody({ card, firstFramePending }: ShotCardBodyProps) {
  const shot = readShot(card);
  const meta = [shot.shot_size, shot.camera, shot.duration_sec > 0 ? `${shot.duration_sec}s` : '']
    .filter(Boolean)
    .join(' · ');

  return (
    <div className="body card-text">
      <ShotFirstFrame assetId={shot.first_frame_asset_id} pending={firstFramePending} />
      {meta && <div className="shot-meta mono">{meta}</div>}
      <p className="shot-desc">{shot.description || '（还没有镜头描述，双击编辑）'}</p>
      {shot.dialogue && <p className="shot-dialogue">「{shot.dialogue}」</p>}
    </div>
  );
}

/**
 * 首帧缩略图。取 preview 而不是 original：镜头卡只有 280 宽，
 * 一条线 12 镜就是 12 张原图，全按原尺寸拉进来会把画布拖垮。
 *
 * 出图中显示骨架而不是留白：这一格的高度是固定的，有没有图版式都不动，
 * 否则一批图陆续回来时整片分镜区会跟着抖十几次。
 */
function ShotFirstFrame({ assetId, pending }: { assetId: string; pending: boolean }) {
  const { data: asset } = useQuery({
    queryKey: ['asset', assetId],
    queryFn: () => api.asset(assetId),
    enabled: assetId !== '',
    staleTime: Infinity,
  });

  if (pending) {
    return (
      <div className="shot-frame" aria-label="首帧生成中">
        <div className="skeleton" />
      </div>
    );
  }
  if (!assetId) return null;

  const preview = asset ? assetPreview(asset) : null;
  return (
    <div className="shot-frame">
      {preview ? <img src={preview} alt="这一镜的首帧" /> : <div className="skeleton" />}
    </div>
  );
}

interface ShotEditorProps {
  card: CanvasCard;
  onSave(shot: ShotParams): void;
  onCancel(): void;
}

export function ShotEditor({ card, onSave, onCancel }: ShotEditorProps) {
  const [draft, setDraft] = useState(() => readShot(card));
  const id = (field: string) => `shot-${field}-${card.id}`;

  function set<K extends keyof ShotParams>(key: K, value: ShotParams[K]): void {
    setDraft((prev) => ({ ...prev, [key]: value }));
  }

  return (
    <div
      className="card-editor"
      onKeyDown={(e) => {
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
