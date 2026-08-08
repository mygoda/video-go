import { Link, useLocation } from 'react-router-dom';
import type { Asset } from '@/api/types';
import { useAssetActions } from '@/hooks/useAssetActions';

interface AssetTileProps {
  asset: Asset;
  /** 资产库瀑布流用高度类拉出参差；任务流里不传，走 task-card 固定高 */
  heightClass?: string;
  showDelete?: boolean;
  linkToDetail?: boolean;
}

function ratioLabel(asset: Asset): string {
  const gcd = (a: number, b: number): number => (b ? gcd(b, a % b) : a);
  const g = gcd(asset.width, asset.height) || 1;
  return `${asset.width / g}:${asset.height / g}`;
}

export function AssetTile({ asset, heightClass, showDelete = true, linkToDetail = true }: AssetTileProps) {
  const location = useLocation();
  const { download, adopt, sendToCanvas, remove } = useAssetActions();

  const media =
    asset.type === 'video' ? (
      <img src={asset.poster ?? asset.thumb_512} alt={asset.source.prompt} loading="lazy" />
    ) : (
      <img src={asset.thumb_512} alt={asset.source.prompt} loading="lazy" />
    );

  return (
    <div className={`task-card${heightClass ? ` ${heightClass}` : ''}`}>
      {linkToDetail ? (
        <Link
          to={`/assets/${asset.id}`}
          // 带上 backgroundLocation：详情走路由级弹层，刷新/直链时再退化成整页
          state={{ backgroundLocation: location }}
          aria-label={`查看资产详情：${asset.source.prompt.slice(0, 20)}`}
        >
          {media}
        </Link>
      ) : (
        media
      )}

      <span className="type-badge">{asset.type === 'video' ? `${asset.duration_seconds ?? 0}s` : ratioLabel(asset)}</span>

      <div className="card-actions">
        <button type="button" className="icon-btn" title="下载" aria-label="下载" onClick={() => download(asset)}>
          ↓
        </button>
        <button type="button" className="icon-btn" title="做同款" aria-label="做同款" onClick={() => adopt(asset)}>
          ⟳
        </button>
        <button
          type="button"
          className="icon-btn"
          title="送入画布"
          aria-label="送入画布"
          onClick={() => void sendToCanvas(asset)}
        >
          ⊹
        </button>
        {showDelete && (
          <button
            type="button"
            className="icon-btn"
            title="删除"
            aria-label="删除"
            style={{ marginLeft: 'auto' }}
            onClick={() => void remove(asset)}
          >
            🗑
          </button>
        )}
      </div>
    </div>
  );
}
