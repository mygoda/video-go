import { useQuery } from '@tanstack/react-query';
import type { Asset } from '@/api/types';

/**
 * text 产物的正文。
 *
 * 它不出现在资产库列表（后端已经把 text 挡在用户可见投影外），但直链详情页和
 * 画布卡片仍然够得着，那两处必须把正文渲染出来 —— 把 `<img src>` 指向一份
 * `text/plain` 得到的只有裂图。内容走 `/api/assets/{id}/content` 的 open 路由，
 * 不需要带鉴权头。
 */
export function AssetTextBody({ asset, className }: { asset: Asset; className?: string }) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ['asset-text', asset.id],
    queryFn: async () => {
      const res = await fetch(asset.original);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return res.text();
    },
    staleTime: Infinity,
  });

  const text = isLoading ? '加载中…' : isError ? '正文读取失败' : (data ?? '').trim() || '（空文本）';

  return (
    <div
      className={className}
      style={{
        width: '100%',
        height: '100%',
        overflow: 'auto',
        padding: 'var(--space-4)',
        background: 'var(--bg-sunken)',
        color: 'var(--text-secondary)',
        fontSize: 'var(--text-sm)',
        lineHeight: 1.7,
        whiteSpace: 'pre-wrap',
        wordBreak: 'break-word',
      }}
    >
      {text}
    </div>
  );
}
