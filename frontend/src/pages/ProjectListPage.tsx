import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { qk, useProjects } from '@/api/queries';
import { api } from '@/api/endpoints';
import { toast } from '@/stores/toast';

const COVER_TONES = ['', 'b', 'c', 'd'];

function relativeTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  const minutes = Math.round(diff / 60_000);
  if (minutes < 1) return '刚刚';
  if (minutes < 60) return `${minutes} 分钟前编辑`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.round(hours / 24);
  if (days === 1) return '昨天';
  if (days < 7) return `${days} 天前`;
  return `${Math.round(days / 7)} 周前`;
}

export function ProjectListPage() {
  const { data: projects, isLoading } = useProjects();
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);

  async function create(): Promise<void> {
    const name = window.prompt('项目名称', '新的短剧');
    if (!name) return;
    setCreating(true);
    try {
      await api.createProject(name);
      await qc.invalidateQueries({ queryKey: qk.projects });
    } catch {
      toast('创建失败，请稍后重试', 'danger');
    } finally {
      setCreating(false);
    }
  }

  return (
    <main className="page">
      <h1 className="page-title">画布项目</h1>
      <p className="page-sub">每个项目是一块无限画布，用来做一部短剧</p>

      {isLoading ? (
        <div className="empty">加载中…</div>
      ) : (
        <div className="proj-grid">
          {/* 新建是排在第一位的虚线占位卡，不是右上角按钮 */}
          <button type="button" className="proj-new" onClick={() => void create()} disabled={creating}>
            ＋<span style={{ fontSize: 'var(--text-sm)' }}>{creating ? '创建中…' : '新建项目'}</span>
          </button>

          {projects?.map((p, i) => (
            <Link className="proj" key={p.id} to={`/canvas/${p.id}`}>
              <div className={`cover art ${COVER_TONES[i % COVER_TONES.length]}`}>
                <span className="type-badge">{p.card_count} 张卡片</span>
              </div>
              <div className="info">
                <div className="name">{p.name}</div>
                <div className="hint">{relativeTime(p.updated_at)}</div>
              </div>
            </Link>
          ))}
        </div>
      )}
    </main>
  );
}
