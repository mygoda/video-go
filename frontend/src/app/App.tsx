import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter } from 'react-router-dom';
import { AppRoutes } from './router';
import { TaskStreamProvider } from '@/realtime/TaskStreamProvider';
import { ToastStack } from '@/components/ToastStack';
import { useAuthStore } from '@/stores/auth';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // 事件通道负责推新数据，窗口聚焦时重拉只会和 setQueryData 打架
      refetchOnWindowFocus: false,
      retry: 1,
      staleTime: 10_000,
    },
  },
});

export function App() {
  const isAuthed = useAuthStore((s) => s.isAuthed);

  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        {/* 事件通道挂在路由之外：任务可能在生成器提交、在画布页完成 */}
        <TaskStreamProvider enabled={isAuthed}>
          <AppRoutes />
          <ToastStack />
        </TaskStreamProvider>
      </BrowserRouter>
    </QueryClientProvider>
  );
}
