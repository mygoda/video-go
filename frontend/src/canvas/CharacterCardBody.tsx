import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { CanvasCard, CharacterParams } from '@/api/types';
import { assetPreview } from '@/api/types';
import { api } from '@/api/endpoints';
import { readCharacter } from './character';

interface CharacterCardBodyProps {
  card: CanvasCard;
  /** 这张卡的定妆图正在出 */
  lookPending: boolean;
}

/**
 * 角色卡的正文：定妆图在上，结构化外观在下。
 *
 * 外观逐项列出而不是连成一段：这几项会被逐字段拼进每一镜的首帧 prompt，
 * 三张图不像时用户要能直接指着看是哪一项没被模型吃进去。连成一段就只剩
 * "整体感觉不对"，查不下去。
 */
export function CharacterCardBody({ card, lookPending }: CharacterCardBodyProps) {
  const c = readCharacter(card);
  const traits = [
    ['年龄', c.age],
    ['体貌', c.build],
    ['发型', c.hair],
    ['衣着', c.outfit],
    ['其他', c.extra],
  ].filter(([, value]) => value.trim() !== '');

  return (
    <div className="body card-text">
      <LookSheet assetId={card.asset_id ?? ''} pending={lookPending} />
      {traits.length ? (
        <dl className="character-traits">
          {traits.map(([label, value]) => (
            <div key={label}>
              <dt>{label}</dt>
              <dd>{value}</dd>
            </div>
          ))}
        </dl>
      ) : (
        <p className="shot-desc">（还没有外观描述，双击编辑）</p>
      )}
    </div>
  );
}

/**
 * 定妆图。用 shot-frame 那一套版式：高度固定，出图中显示骨架而不是留白 ——
 * 否则图回来的那一刻整块画布会抖一下。
 *
 * character-look 只改一件事：图片 contain 而不是 cover。定妆图是全身站姿，
 * 按 16:9 裁剪会把头和脚切掉，而"这个人长什么样"正是这张图存在的全部理由。
 */
function LookSheet({ assetId, pending }: { assetId: string; pending: boolean }) {
  const { data: asset } = useQuery({
    queryKey: ['asset', assetId],
    queryFn: () => api.asset(assetId),
    enabled: assetId !== '',
    staleTime: Infinity,
  });

  if (pending) {
    return (
      <div className="shot-frame character-look" aria-label="定妆图生成中">
        <div className="skeleton" />
      </div>
    );
  }
  if (!assetId) return null;

  const preview = asset ? assetPreview(asset) : null;
  return (
    <div className="shot-frame character-look">
      {preview ? <img src={preview} alt="这个角色的定妆图" /> : <div className="skeleton" />}
    </div>
  );
}

interface CharacterEditorProps {
  card: CanvasCard;
  onSave(c: CharacterParams): void;
  onCancel(): void;
}

export function CharacterEditor({ card, onSave, onCancel }: CharacterEditorProps) {
  const [draft, setDraft] = useState(() => readCharacter(card));
  const id = (field: string) => `character-${field}-${card.id}`;

  function set<K extends keyof CharacterParams>(key: K, value: CharacterParams[K]): void {
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
        <label htmlFor={id('name')}>角色名</label>
        <input
          id={id('name')}
          className="input"
          type="text"
          value={draft.name}
          autoFocus
          onChange={(e) => set('name', e.target.value)}
        />
      </div>

      <label className="card-editor-label" htmlFor={id('age')}>
        年龄与性别
      </label>
      <input
        id={id('age')}
        className="input"
        type="text"
        placeholder="二十五岁女性"
        value={draft.age}
        onChange={(e) => set('age', e.target.value)}
      />

      <label className="card-editor-label" htmlFor={id('build')}>
        体貌
      </label>
      <input
        id={id('build')}
        className="input"
        type="text"
        placeholder="瘦高、鹅蛋脸"
        value={draft.build}
        onChange={(e) => set('build', e.target.value)}
      />

      <label className="card-editor-label" htmlFor={id('hair')}>
        发型
      </label>
      <input
        id={id('hair')}
        className="input"
        type="text"
        placeholder="黑色高马尾"
        value={draft.hair}
        onChange={(e) => set('hair', e.target.value)}
      />

      <label className="card-editor-label" htmlFor={id('outfit')}>
        衣着
      </label>
      <input
        id={id('outfit')}
        className="input"
        type="text"
        placeholder="米色风衣、深色长裤"
        value={draft.outfit}
        onChange={(e) => set('outfit', e.target.value)}
      />

      <label className="card-editor-label" htmlFor={id('extra')}>
        其他特征
      </label>
      <textarea
        id={id('extra')}
        className="textarea"
        style={{ minHeight: 48 }}
        placeholder="画风、配饰、气质"
        value={draft.extra}
        onChange={(e) => set('extra', e.target.value)}
      />
      {/*
        画风提示写在这里而不是替用户塞进 prompt：上游内容审核会以
        "may contain real person" 拒收写实人像，插画风才走得通。这是一条
        用户不可能自己猜到的上游约束，但改成我们悄悄替换他填的画风更糟 ——
        那样他永远不知道自己写的东西没生效。
      */}
      <span className="hint">
        画风写在这里，一并进每一镜的 prompt。写实人像会被上游内容审核拒收，
        建议写「二维赛璐璐插画风」这类
      </span>

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
