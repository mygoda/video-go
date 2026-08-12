import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useLocation } from 'react-router-dom';
import type { CanvasProject } from '@/api/types';
import { api } from '@/api/endpoints';
import { qk, useProjects } from '@/api/queries';
import { readShot } from '@/canvas/shot';

const thumbURL = (id: string) => `/api/assets/${id}/content?variant=thumb_512`;
const posterURL = (id: string) => `/api/assets/${id}/content?variant=poster`;

function Thumb({ assetId, video, caption, badge }: { assetId: string; video?: boolean; caption?: string; badge?: string }) {
  const loc = useLocation();
  return (
    <Link className="short-thumb" to={`/assets/${assetId}`} state={{ backgroundLocation: loc }} title={caption}>
      <img src={video ? posterURL(assetId) : thumbURL(assetId)} loading="lazy" alt={caption ?? ''} />
      {badge && <span className="short-thumb-badge">{badge}</span>}
      {caption && <span className="short-thumb-cap">{caption}</span>}
    </Link>
  );
}

function Section({ label, count, children }: { label: string; count: number; children: React.ReactNode }) {
  return (
    <div className="short-section">
      <div className="short-section-label">
        {label} <span className="mono">{count}</span>
      </div>
      <div className="short-grid">{children}</div>
    </div>
  );
}

/**
 * 一部短剧（= 一个画布项目）。展开后按角色分三块：这条短剧里生成的图片（首帧 /
 * 定妆图）、分镜（每一镜的成片，未出片的退回首帧）、成片（合成的整片）。
 * 画布快照里就带着所有 asset_id，展开时才拉，缩略图直接走 /content 开放路由。
 */
function ShortDramaProject({ project }: { project: CanvasProject }) {
  const [open, setOpen] = useState(false);
  const { data: canvas, isLoading } = useQuery({
    queryKey: qk.canvas(project.id),
    queryFn: () => api.canvas(project.id),
    enabled: open,
  });

  const cards = canvas?.cards ?? [];
  const shots = [...cards.filter((c) => c.kind === 'shot')].sort((a, b) => readShot(a).shot_no - readShot(b).shot_no);
  const composed = cards.filter((c) => c.kind === 'video' && c.refs.length > 0 && c.asset_id);
  const images = [
    ...shots.map((s) => readShot(s).first_frame_asset_id).filter(Boolean),
    ...cards.filter((c) => c.kind === 'character' && c.asset_id).map((c) => c.asset_id as string),
  ];

  return (
    <div className="short-project">
      <button type="button" className="short-head" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        <span className="short-toggle">{open ? '▾' : '▸'}</span>
        {project.cover_asset_id ? (
          <img className="short-cover" src={thumbURL(project.cover_asset_id)} alt="" />
        ) : (
          <span className="short-cover short-cover-empty">🎬</span>
        )}
        <span className="short-name">{project.name || '未命名短剧'}</span>
        <span className="short-sub mono">{project.card_count} 卡</span>
      </button>

      {open && (
        <div className="short-body">
          {isLoading && <div className="hint">加载中…</div>}
          {!isLoading && (
            <>
              <Section label="图片" count={images.length}>
                {images.length ? (
                  images.map((id, i) => <Thumb key={id + i} assetId={id} />)
                ) : (
                  <span className="hint">还没有图片</span>
                )}
              </Section>
              <Section label="分镜" count={shots.length}>
                {shots.length ? (
                  shots.map((s) => {
                    const shot = readShot(s);
                    // 分镜缩略图一律用首帧图（可靠可渲染）；出了片的镜加个 ▶ 角标。
                    // 不用视频封面——缺封面时 /content 会回落成 mp4，塞进 <img> 就是裂图。
                    if (!shot.first_frame_asset_id) return null;
                    return (
                      <Thumb
                        key={s.id}
                        assetId={shot.first_frame_asset_id}
                        caption={`镜${shot.shot_no}`}
                        badge={s.asset_id ? '▶' : undefined}
                      />
                    );
                  })
                ) : (
                  <span className="hint">还没有分镜</span>
                )}
              </Section>
              <Section label="成片" count={composed.length}>
                {composed.length ? (
                  composed.map((c) => <Thumb key={c.id} assetId={c.asset_id as string} video caption="成片" />)
                ) : (
                  <span className="hint">还没有成片</span>
                )}
              </Section>
            </>
          )}
        </div>
      )}
    </div>
  );
}

/** 短剧 tab：按短剧项目组织，每部可展开看它的图片 / 分镜 / 成片。 */
export function ShortDramaList() {
  const { data: projects, isLoading } = useProjects();
  if (isLoading) return <div className="empty">加载中…</div>;
  const items = (projects ?? []).filter((p) => p.card_count > 0);
  if (!items.length) {
    return (
      <div className="empty">
        <div className="title">还没有短剧</div>
        去画布拍一部吧。
      </div>
    );
  }
  return (
    <div className="short-list">
      {items.map((p) => (
        <ShortDramaProject key={p.id} project={p} />
      ))}
    </div>
  );
}
