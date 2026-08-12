import { useState } from 'react';
import { Link, useLocation } from 'react-router-dom';
import type { Task } from '@/api/types';
import { assetPreview } from '@/api/types';
import { useModels, useTasks } from '@/api/queries';
import { formatRelative } from '@/components/admin/format';

const FILTERS = [
  { value: 'all', label: '全部' },
  { value: 'active', label: '进行中' },
  { value: 'succeeded', label: '成功' },
  { value: 'failed', label: '失败' },
];

const MODALITY_LABEL: Record<Task['modality'], string> = {
  image: '图片生成',
  video: '视频生成',
  text: '文本生成',
};

const MODALITY_ICON: Record<Task['modality'], string> = {
  image: '🖼',
  video: '🎬',
  text: '📝',
};

function statusInfo(s: Task['status']): { label: string; cls: string } {
  switch (s) {
    case 'queued':
      return { label: '排队中', cls: 'muted' };
    case 'running':
      return { label: '生成中', cls: 'info' };
    case 'succeeded':
      return { label: '成功', cls: 'ok' };
    case 'failed':
      return { label: '失败', cls: 'danger' };
    case 'canceled':
      return { label: '已取消', cls: 'muted' };
    default:
      return { label: s, cls: 'muted' };
  }
}

/** 我的任务：当前用户自己提交的全部生成任务（含画布里的首帧 / 出片 / 合成），
 *  带状态与失败原因。与 /admin/tasks 的全站监控无关——那只有管理员能看。 */
export function UserTasksPage() {
  const [filter, setFilter] = useState('all');
  const loc = useLocation();
  const { data: tasks, isLoading } = useTasks();
  const { data: imageModels } = useModels('image');
  const { data: videoModels } = useModels('video');
  const { data: textModels } = useModels('text');
  const modelName = new Map(
    [...(imageModels ?? []), ...(videoModels ?? []), ...(textModels ?? [])].map((m) => [m.id, m.name]),
  );

  const rows = (tasks ?? []).filter((t) => {
    if (filter === 'all') return true;
    if (filter === 'active') return t.status === 'queued' || t.status === 'running';
    if (filter === 'failed') return t.status === 'failed' || t.status === 'canceled';
    return t.status === filter;
  });

  return (
    <main className="page" style={{ maxWidth: 900 }}>
      <h1 className="page-title">我的任务</h1>
      <p className="page-sub">你提交的全部生成任务 · 最近 100 条</p>

      <div className="filter-bar">
        {FILTERS.map((f) => (
          <button
            key={f.value}
            type="button"
            className="chip"
            aria-pressed={filter === f.value}
            style={filter === f.value ? { borderColor: 'var(--accent)', background: 'var(--accent-subtle)' } : undefined}
            onClick={() => setFilter(f.value)}
          >
            <span className="v">{f.label}</span>
          </button>
        ))}
      </div>

      {isLoading && <div className="empty">加载中…</div>}
      {!isLoading && rows.length === 0 && (
        <div className="empty">
          <div className="title">没有任务</div>
          去生成器或画布做点什么吧。
        </div>
      )}

      <div className="task-list">
        {rows.map((t) => {
          const st = statusInfo(t.status);
          const asset = t.assets?.[0];
          const preview = asset ? assetPreview(asset) : null;
          return (
            <div className="task-row" key={t.id}>
              <div className="task-thumb">
                {preview ? (
                  <Link to={`/assets/${asset!.id}`} state={{ backgroundLocation: loc }}>
                    <img src={preview} alt="" loading="lazy" />
                  </Link>
                ) : (
                  <span className="task-thumb-ph">{MODALITY_ICON[t.modality]}</span>
                )}
              </div>
              <div className="task-main">
                <div className="task-row-title">
                  {MODALITY_LABEL[t.modality]}
                  {modelName.get(t.model_id) ? ` · ${modelName.get(t.model_id)}` : ''}
                </div>
                <div className="task-row-prompt">{t.prompt || '—'}</div>
                {t.status === 'failed' && t.error?.message && (
                  <div className="task-row-err">失败原因：{t.error.message}</div>
                )}
              </div>
              <div className="task-row-side">
                <span className={`task-badge ${st.cls}`}>{st.label}</span>
                <span className="task-row-meta mono">
                  ⚡{t.estimated_cost} · {formatRelative(t.created_at)}
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </main>
  );
}
