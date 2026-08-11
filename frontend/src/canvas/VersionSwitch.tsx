import type { CanvasCard } from '@/api/types';

interface VersionSwitchProps {
  card: CanvasCard;
  onSelectVersion(cardId: string, assetId: string): void;
}

/**
 * 产物版本切换器 ‹当前/总数›。落在画面（成片 / 图）的左下角：它说的是「这一格
 * 的画面是第几版」，贴着画面才对得上；挂到整张卡的底边会压住下面的镜头描述。
 *
 * 只在有两版及以上时出现。当前版 = asset_id 在 history 里的下标；点箭头把 asset_id
 * 指到相邻那一版（走 card.update，history 不动，只挪当前指针）。
 */
export function VersionSwitch({ card, onSelectVersion }: VersionSwitchProps) {
  const hist = card.history ?? [];
  if (hist.length <= 1) return null;

  const found = hist.findIndex((h) => h.asset_id === card.asset_id);
  // 当前 asset 不在历史里（老数据）就当作停在最新那一版
  const idx = found < 0 ? hist.length - 1 : found;
  const go = (to: number) => {
    if (to < 0 || to >= hist.length || to === idx) return;
    onSelectVersion(card.id, hist[to].asset_id);
  };

  return (
    // stopPropagation：点切版按钮不该顺带选中 / 拖动这张卡
    <span className="version-switch" onPointerDown={(e) => e.stopPropagation()}>
      <button
        type="button"
        className="ver-btn"
        aria-label="上一版"
        title="上一版"
        disabled={idx <= 0}
        onClick={() => go(idx - 1)}
      >
        ‹
      </button>
      <span className="mono">
        {idx + 1}/{hist.length}
      </span>
      <button
        type="button"
        className="ver-btn"
        aria-label="下一版"
        title="下一版"
        disabled={idx >= hist.length - 1}
        onClick={() => go(idx + 1)}
      >
        ›
      </button>
    </span>
  );
}
