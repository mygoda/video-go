import type { ReactNode } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { useMe } from '@/api/queries';
import { useAuthStore } from '@/stores/auth';

/**
 * 管理端路由守卫。这只是第一道 —— 后端对 /api/admin/* 同样按 role 拦截，
 * 前端这层只负责别让普通用户看到一个注定 403 的空壳页面。
 */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const { data: me, isPending } = useMe(isAuthed);
  const location = useLocation();

  if (!isAuthed) {
    const next = encodeURIComponent(location.pathname + location.search);
    return <Navigate to={`/login?next=${next}`} replace />;
  }

  // 角色未知时不能先渲染再踢走，否则管理端内容会闪一下
  if (isPending) return <div className="empty">正在校验权限…</div>;

  if (me?.role !== 'admin') {
    return (
      <div className="empty" role="alert">
        <div className="title">没有访问权限</div>
        <p>管理后台仅对管理员开放。如果你认为这是误判，请联系平台管理员。</p>
      </div>
    );
  }

  return <>{children}</>;
}
