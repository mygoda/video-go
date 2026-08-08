import { NavLink, Outlet, useLocation } from 'react-router-dom';

const ADMIN_NAV = [
  { to: '/admin/models', label: '模型与供应商' },
  { to: '/admin/users', label: '用户与积分' },
  { to: '/admin/tasks', label: '任务监控' },
  { to: '/admin/storage', label: '存储配额' },
];

export function AdminLayout() {
  const { pathname } = useLocation();

  return (
    <main className="page admin">
      <h1 className="page-title">管理后台</h1>
      <p className="page-sub">改配置即时生效，不需要改代码、不需要重启后端。</p>

      <nav className="admin-nav" aria-label="管理后台">
        {ADMIN_NAV.map((item) => (
          <NavLink
            key={item.to}
            className="admin-nav-item"
            to={item.to}
            aria-current={pathname.startsWith(item.to) ? 'page' : undefined}
          >
            {item.label}
          </NavLink>
        ))}
      </nav>

      <Outlet />
    </main>
  );
}
