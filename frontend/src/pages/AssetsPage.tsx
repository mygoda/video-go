import { useState } from 'react';
import { Link, useLocation } from 'react-router-dom';
import type { Asset } from '@/api/types';
import { useAssets, useMe } from '@/api/queries';
import { useAuthStore } from '@/stores/auth';

const TYPES = [
  { value: 'all', label: '全部' },
  { value: 'image', label: '图片' },
  { value: 'video', label: '视频' },
];

/** 瀑布流靠固定高度档位拉出参差，跟高保真稿一致；真实高度由 width/height 推算 */
function heightClass(asset: Asset): string {
  const ratio = asset.height / Math.max(asset.width, 1);
  if (ratio >= 1.5) return 'h-280';
  if (ratio >= 1.2) return 'h-240';
  if (ratio >= 0.95) return 'h-220';
  if (ratio >= 0.7) return 'h-190';
  return 'h-160';
}

function formatBytes(bytes: number): string {
  return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
}

export function AssetsPage() {
  const [type, setType] = useState('all');
  const location = useLocation();
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const { data: me } = useMe(isAuthed);
  const { query, loadMore } = useAssets(type);

  const page = query.data;

  return (
    <main className="page">
      <h1 className="page-title">资产库</h1>
      <p className="page-sub">全部生成产物 · 游标分页，滚动加载</p>

      <div className="filter-bar">
        {TYPES.map((t) => (
          <button
            key={t.value}
            type="button"
            className="chip"
            aria-pressed={type === t.value}
            style={type === t.value ? { borderColor: 'var(--accent)', background: 'var(--accent-subtle)' } : undefined}
            onClick={() => setType(t.value)}
          >
            <span className="v">{t.label}</span>
          </button>
        ))}
        <span className="hint" style={{ marginLeft: 'auto' }}>
          共 <span className="mono">{page?.total ?? 0}</span> 项
          {me && (
            <>
              {' '}
              · 已用 <span className="mono">{formatBytes(me.storage_used)}</span>
            </>
          )}
        </span>
      </div>

      {query.isLoading && <div className="empty">加载中…</div>}

      {page && page.items.length === 0 && (
        <div className="empty">
          <div className="title">这里还是空的</div>
          去生成器做点什么吧。
        </div>
      )}

      <div className="masonry">
        {page?.items.map((asset) => (
          <Link
            key={asset.id}
            className="asset"
            to={`/assets/${asset.id}`}
            state={{ backgroundLocation: location }}
            style={{ display: 'block' }}
          >
            <img
              className={`art ${heightClass(asset)}`}
              style={{ width: '100%', objectFit: 'cover' }}
              src={asset.type === 'video' ? (asset.poster ?? asset.thumb_512) : asset.thumb_512}
              alt={asset.source.prompt}
              loading="lazy"
            />
            {/* 静止时只有画面，hover 才出元信息 */}
            <div className="asset-meta">
              {asset.type === 'video' ? `▶ ${asset.duration_seconds ?? 0}s` : asset.source.prompt}
              <br />
              <span className="mono">{asset.source.model_name}</span>
            </div>
          </Link>
        ))}
      </div>

      {page?.next_cursor && (
        <div style={{ textAlign: 'center', marginTop: 'var(--space-6)' }}>
          <button type="button" className="btn" disabled={loadMore.isPending} onClick={() => loadMore.mutate()}>
            {loadMore.isPending ? '加载中…' : '加载更多'}
          </button>
        </div>
      )}
    </main>
  );
}
