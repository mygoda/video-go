import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { Asset, Me, Paged, Task } from './types';
import { adminApi, api } from './endpoints';

export const qk = {
  me: ['me'] as const,
  credits: ['credits'] as const,
  models: (modality: 'image' | 'video') => ['models', modality] as const,
  skills: ['skills'] as const,
  tasks: ['tasks'] as const,
  assets: (type: string) => ['assets', type] as const,
  projects: ['projects'] as const,
  canvas: (id: string) => ['canvas', id] as const,
  adminProviders: ['admin', 'providers'] as const,
  adminModels: ['admin', 'models'] as const,
  adminModel: (id: string) => ['admin', 'models', id] as const,
  adminUsers: (q: string) => ['admin', 'users', q] as const,
  adminUserLedger: (id: string) => ['admin', 'users', id, 'ledger'] as const,
  adminTasks: (status: string, errorCode: string) => ['admin', 'tasks', status, errorCode] as const,
  adminTaskStats: (window: string) => ['admin', 'tasks', 'stats', window] as const,
  adminBreakers: ['admin', 'breakers'] as const,
  adminStorage: ['admin', 'storage'] as const,
};

export function useMe(enabled: boolean) {
  return useQuery({ queryKey: qk.me, queryFn: api.me, enabled, staleTime: 30_000 });
}

export function useModels(modality: 'image' | 'video') {
  return useQuery({
    queryKey: qk.models(modality),
    queryFn: () => api.models(modality),
    staleTime: 5 * 60_000,
  });
}

export function useSkills() {
  return useQuery({ queryKey: qk.skills, queryFn: api.skills, staleTime: 5 * 60_000 });
}

export function useTasks() {
  return useQuery({ queryKey: qk.tasks, queryFn: api.tasks, staleTime: Infinity });
}

export function useCreditLedger() {
  return useQuery({ queryKey: qk.credits, queryFn: api.creditLedger });
}

export function useProjects() {
  return useQuery({ queryKey: qk.projects, queryFn: api.projects });
}

export function useCanvas(projectId: string) {
  return useQuery({ queryKey: qk.canvas(projectId), queryFn: () => api.canvas(projectId) });
}

/**
 * 资产库分页。列表可能很长且只往后翻，用游标累积而不是 useInfiniteQuery ——
 * 事件到达时要能直接 setQueryData 往头部插一条新资产，扁平结构改起来简单得多。
 */
export function useAssets(type: string) {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: qk.assets(type),
    queryFn: () => api.assets({ type }),
  });

  const loadMore = useMutation({
    mutationFn: async () => {
      const current = qc.getQueryData<Paged<Asset> & { total: number }>(qk.assets(type));
      if (!current?.next_cursor) return null;
      return api.assets({ type, cursor: current.next_cursor });
    },
    onSuccess: (page) => {
      if (!page) return;
      qc.setQueryData<Paged<Asset> & { total: number }>(qk.assets(type), (prev) =>
        prev ? { ...page, items: [...prev.items, ...page.items] } : page,
      );
    },
  });

  return { query, loadMore };
}

export function mergeTask(qc: ReturnType<typeof useQueryClient>, patch: Partial<Task> & { id: string }) {
  qc.setQueryData<Task[]>(qk.tasks, (prev) => {
    if (!prev) return prev;
    let found = false;
    const next = prev.map((t) => {
      if (t.id !== patch.id) return t;
      found = true;
      return { ...t, ...patch };
    });
    return found ? next : prev;
  });
}

export function patchMe(qc: ReturnType<typeof useQueryClient>, patch: Partial<Me>) {
  qc.setQueryData<Me>(qk.me, (prev) => (prev ? { ...prev, ...patch } : prev));
}

/* ─────────────────────────────── 管理后台 ─────────────────────────────── */

export function useAdminProviders() {
  return useQuery({ queryKey: qk.adminProviders, queryFn: adminApi.providers });
}

export function useAdminModels() {
  return useQuery({ queryKey: qk.adminModels, queryFn: adminApi.models });
}

export function useAdminModel(id: string | undefined) {
  return useQuery({
    queryKey: qk.adminModel(id ?? ''),
    queryFn: () => adminApi.model(id as string),
    enabled: Boolean(id),
  });
}

export function useAdminUsers(q: string) {
  return useQuery({ queryKey: qk.adminUsers(q), queryFn: () => adminApi.users(q) });
}

export function useAdminUserLedger(id: string | null) {
  return useQuery({
    queryKey: qk.adminUserLedger(id ?? ''),
    queryFn: () => adminApi.userLedger(id as string),
    enabled: Boolean(id),
  });
}

export function useAdminTasks(status: string, errorCode: string) {
  return useQuery({
    queryKey: qk.adminTasks(status, errorCode),
    queryFn: () => adminApi.tasks({ status: status || undefined, error_code: errorCode || undefined }),
  });
}

export function useAdminTaskStats(window: string) {
  return useQuery({ queryKey: qk.adminTaskStats(window), queryFn: () => adminApi.taskStats(window) });
}

export function useAdminBreakers() {
  return useQuery({ queryKey: qk.adminBreakers, queryFn: adminApi.circuitBreakers });
}

export function useAdminStorage() {
  return useQuery({ queryKey: qk.adminStorage, queryFn: adminApi.storageUsage });
}

/**
 * 模型配置改完要让生成器的模型下拉立刻跟着变 —— 这正是管理后台存在的理由，
 * 所以两个 modality 的目录缓存一并作废，而不是等 staleTime 到期。
 */
export function invalidateModelCatalog(qc: ReturnType<typeof useQueryClient>): void {
  void qc.invalidateQueries({ queryKey: qk.adminModels });
  void qc.invalidateQueries({ queryKey: ['models'] });
}
