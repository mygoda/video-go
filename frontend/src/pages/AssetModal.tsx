import { useEffect, useRef } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '@/api/endpoints';
import { formatMediaDuration } from '@/components/admin/format';
import { useAssetActions } from '@/hooks/useAssetActions';

/**
 * 资产详情。列表里点开是路由级弹层（背景保留列表），直接访问 URL 时 standalone
 * 渲染成整页 —— 分享出去的链接不该是一个悬在空白上的弹层。
 */
export function AssetModal({ standalone }: { standalone?: boolean }) {
  const { assetId } = useParams<{ assetId: string }>();
  const navigate = useNavigate();
  const closeRef = useRef<HTMLButtonElement>(null);
  const { download, adopt, sendToCanvas, remove } = useAssetActions();

  const { data: asset, isLoading } = useQuery({
    queryKey: ['asset', assetId],
    queryFn: () => api.asset(assetId!),
    enabled: Boolean(assetId),
  });

  const close = () => (standalone ? navigate('/assets') : navigate(-1));

  useEffect(() => {
    if (standalone) return;
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') navigate(-1);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [standalone, navigate]);

  const body = isLoading ? (
    <div className="empty">加载中…</div>
  ) : !asset ? (
    <div className="empty">
      <div className="title">资产不存在或已删除</div>
    </div>
  ) : (
    <div className="modal">
      <div className="modal-media">
        {asset.type === 'video' ? (
          <video src={asset.original} poster={asset.poster ?? undefined} controls playsInline />
        ) : (
          <img src={asset.original} alt={asset.source.prompt} />
        )}
      </div>
      <div className="modal-side">
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
          <h2 style={{ margin: 0, fontSize: 'var(--text-md)' }}>{asset.source.model_name ?? asset.source.model_id}</h2>
          <button ref={closeRef} type="button" className="icon-btn" aria-label="关闭" style={{ marginLeft: 'auto', position: 'static' }} onClick={close}>
            ×
          </button>
        </div>

        <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--text-secondary)' }}>{asset.source.prompt}</p>

        <div className="panel" style={{ padding: 'var(--space-3)' }}>
          {Object.entries(asset.source.params).map(([k, v]) => (
            <div className="kv" key={k}>
              <span className="k">{k}</span>
              <span className="v mono">{String(v)}</span>
            </div>
          ))}
          {asset.width && asset.height && (
            <div className="kv">
              <span className="k">尺寸</span>
              <span className="v mono">
                {asset.width}×{asset.height}
              </span>
            </div>
          )}
          {formatMediaDuration(asset.duration_ms) && (
            <div className="kv">
              <span className="k">时长</span>
              <span className="v mono">{formatMediaDuration(asset.duration_ms)}</span>
            </div>
          )}
        </div>

        <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap', marginTop: 'auto' }}>
          <button type="button" className="btn btn-primary" onClick={() => adopt(asset)}>
            做同款
          </button>
          <button type="button" className="btn" onClick={() => download(asset)}>
            下载
          </button>
          <button type="button" className="btn" onClick={() => void sendToCanvas(asset)}>
            送入画布
          </button>
          <button
            type="button"
            className="btn"
            onClick={async () => {
              await remove(asset);
              close();
            }}
          >
            删除
          </button>
        </div>
      </div>
    </div>
  );

  if (standalone) return <main className="page">{body}</main>;

  return (
    <div
      className="modal-backdrop"
      role="dialog"
      aria-modal="true"
      aria-label="资产详情"
      onClick={(e) => {
        if (e.target === e.currentTarget) close();
      }}
    >
      {body}
    </div>
  );
}
