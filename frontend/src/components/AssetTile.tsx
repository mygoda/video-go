import { Link, useLocation } from 'react-router-dom';
import type { Asset } from '@/api/types';
import { assetPreview } from '@/api/types';
import { AssetTextBody } from '@/components/AssetTextBody';
import { formatMediaDuration } from '@/components/admin/format';
import { useAssetActions } from '@/hooks/useAssetActions';

interface AssetTileProps {
  asset: Asset;
  /** 资产库瀑布流用高度类拉出参差；任务流里不传，走 task-card 固定高 */
  heightClass?: string;
  showDelete?: boolean;
  linkToDetail?: boolean;
}

/** 尺寸未知（text 产物、或后端没回宽高）时返回 null，让角标整个不出现，别渲染成 NaN:NaN */
function ratioLabel(asset: Asset): string | null {
  if (!asset.width || !asset.height) return null;
  const gcd = (a: number, b: number): number => (b ? gcd(b, a % b) : a);
  const g = gcd(asset.width, asset.height) || 1;
  return `${asset.width / g}:${asset.height / g}`;
}

export function AssetTile({ asset, heightClass, showDelete = true, linkToDetail = true }: AssetTileProps) {
  const location = useLocation();
  const { download, adopt, sendToCanvas, remove } = useAssetActions();

  const preview = assetPreview(asset);
  const media = preview ? (
    <img src={preview} alt={asset.source.prompt} loading="lazy" />
  ) : (
    <AssetTextBody asset={asset} className="art" />
  );

  const badge = asset.type === 'video' ? formatMediaDuration(asset.duration_ms) : ratioLabel(asset);

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

      {badge && <span className="type-badge">{badge}</span>}

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
