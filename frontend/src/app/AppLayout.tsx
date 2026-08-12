import { NavLink, Outlet, useLocation } from 'react-router-dom';
import { useMe } from '@/api/queries';
import { useAuthStore } from '@/stores/auth';
import { useStreamStatus } from '@/realtime/TaskStreamProvider';

const NAV = [
  { to: '/create/image', label: '生成器', match: '/create' },
  { to: '/projects', label: '画布', match: '/projects' },
  { to: '/assets', label: '资产', match: '/assets' },
];

export function AppLayout() {
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const { data: me } = useMe(isAuthed);
  const { degraded } = useStreamStatus();
  const { pathname } = useLocation();

  return (
    <>
      <header className="topbar">
        <div className="logo">
          <span className="logo-mark">D</span> dreamVideo
        </div>
        <nav className="nav">
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              className="nav-item"
              to={item.to}
              aria-current={pathname.startsWith(item.match) ? 'page' : undefined}
            >
              {item.label}
            </NavLink>
          ))}
          {/* 普通用户看不到入口，直接敲 URL 也会被 RequireAdmin 和后端各挡一次 */}
          {me?.role === 'admin' && (
            <NavLink
              className="nav-item"
              to="/admin/models"
              aria-current={pathname.startsWith('/admin') ? 'page' : undefined}
            >
              管理
            </NavLink>
          )}
        </nav>
        <div className="topbar-right">
          {degraded && (
            <span className="degraded" title="实时推送不可用，已改为定时拉取，功能不受影响">
              实时连接已降级
            </span>
          )}
          {isAuthed ? (
            <>
              <NavLink className="credit-badge" to="/me" aria-label={`积分余额 ${me?.credits ?? 0}`}>
                ⚡ <span className="mono">{(me?.credits ?? 0).toLocaleString()}</span>
              </NavLink>
              <NavLink className="avatar" to="/me" aria-label="个人中心">
                {me?.username?.[0]?.toUpperCase() ?? '·'}
              </NavLink>
            </>
          ) : (
            // 未登录是另一套顶栏，不是灰掉的按钮
            <NavLink className="btn btn-primary" to="/login">
              登录
            </NavLink>
          )}
        </div>
      </header>
      <Outlet />
    </>
  );
}
