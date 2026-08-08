import { Suspense, lazy } from 'react';
import { Navigate, Route, Routes, useLocation, type Location } from 'react-router-dom';
import { AppLayout } from './AppLayout';
import { AdminLayout } from './AdminLayout';
import { RequireAdmin } from './RequireAdmin';
import { RequireAuth } from './RequireAuth';
import { LoginPage } from '@/pages/LoginPage';
import { GeneratorPage } from '@/pages/GeneratorPage';
import { AssetsPage } from '@/pages/AssetsPage';
import { AssetModal } from '@/pages/AssetModal';
import { ProfilePage } from '@/pages/ProfilePage';
import { ProjectListPage } from '@/pages/ProjectListPage';

// 画布是唯一重到值得单独切包的页面（frontend-design.md §8.1）
const CanvasPage = lazy(() => import('@/pages/CanvasPage').then((m) => ({ default: m.CanvasPage })));

// 管理端只有管理员会打开，没理由让所有人下载它的 capability schema 编辑器
const AdminModelsPage = lazy(() =>
  import('@/pages/admin/AdminModelsPage').then((m) => ({ default: m.AdminModelsPage })),
);
const AdminUsersPage = lazy(() =>
  import('@/pages/admin/AdminUsersPage').then((m) => ({ default: m.AdminUsersPage })),
);
const AdminTasksPage = lazy(() =>
  import('@/pages/admin/AdminTasksPage').then((m) => ({ default: m.AdminTasksPage })),
);
const AdminStoragePage = lazy(() =>
  import('@/pages/admin/AdminStoragePage').then((m) => ({ default: m.AdminStoragePage })),
);

interface BackgroundState {
  backgroundLocation?: Location;
}

export function AppRoutes() {
  const location = useLocation();
  const background = (location.state as BackgroundState | null)?.backgroundLocation;

  return (
    <>
      <Routes location={background ?? location}>
        <Route path="/login" element={<LoginPage />} />
        {/* 画布走自己的窄顶栏布局，不套 AppLayout —— 这里视口比导航值钱 */}
        <Route
          path="/canvas/:projectId"
          element={
            <RequireAuth>
              <Suspense fallback={<div className="empty">画布加载中…</div>}>
                <CanvasPage />
              </Suspense>
            </RequireAuth>
          }
        />
        <Route element={<AppLayout />}>
          <Route index element={<Navigate to="/create/image" replace />} />
          <Route path="/create" element={<Navigate to="/create/image" replace />} />
          <Route path="/create/image" element={<GeneratorPage modality="image" />} />
          <Route path="/create/video" element={<GeneratorPage modality="video" />} />
          <Route
            path="/assets"
            element={
              <RequireAuth>
                <AssetsPage />
              </RequireAuth>
            }
          />
          {/* 直接访问详情 URL 时没有 background，渲染成整页而不是弹层 */}
          <Route
            path="/assets/:assetId"
            element={
              <RequireAuth>
                <AssetModal standalone />
              </RequireAuth>
            }
          />
          <Route
            path="/me"
            element={
              <RequireAuth>
                <ProfilePage />
              </RequireAuth>
            }
          />
          <Route
            path="/projects"
            element={
              <RequireAuth>
                <ProjectListPage />
              </RequireAuth>
            }
          />
          <Route
            path="/admin"
            element={
              <RequireAdmin>
                <Suspense fallback={<div className="empty">管理后台加载中…</div>}>
                  <AdminLayout />
                </Suspense>
              </RequireAdmin>
            }
          >
            <Route index element={<Navigate to="/admin/models" replace />} />
            <Route path="models" element={<AdminModelsPage />} />
            <Route path="users" element={<AdminUsersPage />} />
            <Route path="tasks" element={<AdminTasksPage />} />
            <Route path="storage" element={<AdminStoragePage />} />
          </Route>
          <Route path="*" element={<div className="empty"><div className="title">页面不存在</div></div>} />
        </Route>
      </Routes>

      {background && (
        <Routes>
          <Route path="/assets/:assetId" element={<AssetModal />} />
        </Routes>
      )}
    </>
  );
}
