import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { CanvasCard, ShotParams } from '@/api/types';
import { assetPreview } from '@/api/types';
import { api } from '@/api/endpoints';
import { cardTitle } from './cardTitle';
import { readShot } from './shot';
import { VersionSwitch } from './VersionSwitch';

interface ShotCardBodyProps {
  card: CanvasCard;
  /**
   * 画布缩放。`<video>` 很贵，缩小时只挂一张封面图 ——
   * 与 CanvasCardView 对 video 卡的同一条规则，阈值也取同一个。
   */
  k: number;
  /** 这一镜的首帧正在出。批量与单张重出都会把它翻成 true */
  firstFramePending: boolean;
  /** 这一镜的成片正在出 */
  renderPending: boolean;
  /** 切换成片版本（把 asset_id 指到 history 里某一版） */
  onSelectVersion(cardId: string, assetId: string): void;
}

/**
 * 镜头卡的正文：镜号在卡片标题里，这里是画面、画面描述与台词。
 *
 * 台词单独占一块而不是和描述连排：它是出片时唯一会被念出来的部分，
 * 混在描述里用户就看不出这一镜到底有没有人说话。
 *
 * 画面排在最上面：首帧出完那一步要停下来等用户逐张确认，出片出完也要停下来
 * 等他逐条看，而「确认」就是看画面，画面得比描述先进入视线。
 */
export function ShotCardBody({ card, k, firstFramePending, renderPending, onSelectVersion }: ShotCardBodyProps) {
  const shot = readShot(card);
  const meta = [shot.shot_size, shot.camera, shot.duration_sec > 0 ? `${shot.duration_sec}s` : '']
    .filter(Boolean)
    .join(' · ');

  return (
    <div className="body card-text">
      <ShotFrame
        card={card}
        firstFrameAssetId={shot.first_frame_asset_id}
        videoAssetId={card.asset_id ?? ''}
        k={k}
        firstFramePending={firstFramePending}
        renderPending={renderPending}
        onSelectVersion={onSelectVersion}
      />
      {meta && <div className="shot-meta mono">{meta}</div>}
      <p className="shot-desc">{shot.description || '（还没有镜头描述，双击编辑）'}</p>
      {shot.dialogue && (
        <p className="shot-dialogue">
          {shot.voice === false && (
            <span className="mono" title="这一镜的台词不会进出片 prompt，成片只有环境音">
              🔇{' '}
            </span>
          )}
          「{shot.dialogue}」
        </p>
      )}
    </div>
  );
}

interface ShotFrameProps {
  card: CanvasCard;
  firstFrameAssetId: string;
  videoAssetId: string;
  k: number;
  firstFramePending: boolean;
  renderPending: boolean;
  onSelectVersion(cardId: string, assetId: string): void;
}

/**
 * 这一镜的画面：有成片就放成片，否则放首帧。
 *
 * 两件产物同时存在且各占各的字段（首帧在 params.first_frame_asset_id，成片在
 * card.asset_id），这里只决定**显示哪一件** —— 成片一出来，用户要看的就是它，
 * 而首帧仍然留在数据里，▶ 重出成片时喂进去的还是同一张。
 *
 * 取 preview 而不是 original：镜头卡只有 280 宽，一条线 12 镜全按原尺寸拉进来
 * 会把画布拖垮。
 *
 * 出片 / 出图中显示骨架而不是留白：这一格的高度是固定的，有没有画面版式都不动，
 * 否则一批产物陆续回来时整片分镜区会跟着抖十几次。
 */
function ShotFrame({ card, firstFrameAssetId, videoAssetId, k, firstFramePending, renderPending, onSelectVersion }: ShotFrameProps) {
  const { data: firstFrame } = useQuery({
    queryKey: ['asset', firstFrameAssetId],
    queryFn: () => api.asset(firstFrameAssetId),
    enabled: firstFrameAssetId !== '',
    staleTime: Infinity,
  });
  const { data: video } = useQuery({
    queryKey: ['asset', videoAssetId],
    queryFn: () => api.asset(videoAssetId),
    enabled: videoAssetId !== '',
    staleTime: Infinity,
  });

  if (renderPending) {
    return (
      <div className="shot-frame" aria-label="成片生成中">
        <div className="skeleton" />
      </div>
    );
  }
  if (firstFramePending) {
    return (
      <div className="shot-frame" aria-label="首帧生成中">
        <div className="skeleton" />
      </div>
    );
  }

  if (videoAssetId) {
    const poster = video ? assetPreview(video) : null;
    return (
      <div className="shot-frame">
        {!video ? (
          <div className="skeleton" />
        ) : k >= 0.6 ? (
          <video src={video.original} poster={video.poster ?? undefined} controls playsInline preload="none" />
        ) : poster ? (
          <img src={poster} alt="这一镜的成片" />
        ) : (
          <div className="skeleton" />
        )}
        <VersionSwitch card={card} onSelectVersion={onSelectVersion} />
      </div>
    );
  }

  if (!firstFrameAssetId) return null;

  const preview = firstFrame ? assetPreview(firstFrame) : null;
  return (
    <div className="shot-frame">
      {preview ? <img src={preview} alt="这一镜的首帧" /> : <div className="skeleton" />}
    </div>
  );
}

interface ShotEditorProps {
  card: CanvasCard;
  /** 这条创作线上全部角色卡，逐镜勾选谁出场 */
  characters: CanvasCard[];
  onSave(shot: ShotParams): void;
  onCancel(): void;
}

export function ShotEditor({ card, characters, onSave, onCancel }: ShotEditorProps) {
  const [draft, setDraft] = useState(() => readShot(card));
  const id = (field: string) => `shot-${field}-${card.id}`;

  function set<K extends keyof ShotParams>(key: K, value: ShotParams[K]): void {
    setDraft((prev) => ({ ...prev, [key]: value }));
  }

  function toggleCharacter(id: string): void {
    setDraft((prev) => ({
      ...prev,
      character_ids: prev.character_ids.includes(id)
        ? prev.character_ids.filter((x) => x !== id)
        : [...prev.character_ids, id],
    }));
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

      {/*
        逐镜勾选出场角色，而不是"这条线上的角色全拼进每一镜"：一部短剧里不是
        每一镜都有每个人，把没出场的人的外观喂进 prompt，模型会照着把他画进画面。

        这条线上一个角色卡都没有时整块不渲染 —— 一个永远空着的勾选区只会让用户
        以为功能坏了。建角色卡在顶栏。
      */}
      {characters.length > 0 && (
        <>
          <span className="card-editor-label">出场角色</span>
          <div className="character-picker">
            {characters.map((c) => (
              <label key={c.id} className="character-pick">
                <input
                  type="checkbox"
                  checked={draft.character_ids.includes(c.id)}
                  onChange={() => toggleCharacter(c.id)}
                />
                {cardTitle(c)}
              </label>
            ))}
          </div>
          <span className="hint">勾中的角色，外观描述会逐项拼进这一镜的首帧图 prompt</span>
        </>
      )}

      {/*
        三个选项而不是一个勾选框：「跟随默认」是一个真实且不同的状态，
        表示这一镜还没人单独表过态、流程条上那个全局开关说了算。做成勾选框
        就只剩开与关，用户一旦碰过任何一镜，那一镜就永远脱离全局开关，
        而他并不知道自己做了这个决定。
      */}
      <label className="card-editor-label" htmlFor={id('voice')}>
        人声
      </label>
      <select
        id={id('voice')}
        className="input"
        value={draft.voice === null ? 'default' : draft.voice ? 'on' : 'off'}
        onChange={(e) => set('voice', e.target.value === 'default' ? null : e.target.value === 'on')}
      >
        <option value="default">跟随默认</option>
        <option value="on">念出来</option>
        <option value="off">不念（只留环境音）</option>
      </select>
      <span className="hint">改这个要重出这一镜才生效；台词本身照旧留着，字幕仍然取它</span>

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
