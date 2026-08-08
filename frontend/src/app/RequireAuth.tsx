import type { ReactNode } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { useAuthStore } from '@/stores/auth';

export function RequireAuth({ children }: { children: ReactNode }) {
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const location = useLocation();
  if (!isAuthed) {
    const next = encodeURIComponent(location.pathname + location.search);
    return <Navigate to={`/login?next=${next}`} replace />;
  }
  return <>{children}</>;
}
