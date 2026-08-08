import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { adminApi } from '@/api/endpoints';
import { useAdminBreakers, useAdminTaskStats, useAdminTasks } from '@/api/queries';
import type { AdminTask, TaskErrorCode } from '@/api/types';
import { formatDuration, formatRelative } from '@/components/admin/format';
import { presentFailure } from '@/components/task/failure';
import { toast } from '@/stores/toast';

const WINDOWS = [
  { value: '1h', label: '最近 1 小时' },
  { value: '24h', label: '最近 24 小时' },
  { value: '7d', label: '最近 7 天' },
];

const STATUSES = [
  { value: '', label: '全部状态' },
  { value: 'queued', label: '排队中' },
  { value: 'running', label: '生成中' },
  { value: 'succeeded', label: '已完成' },
  { value: 'failed', label: '失败' },
  { value: 'canceled', label: '已取消' },
];

/** 失败三分类，DEM-63 要求单独看得见。标题与配色一律走 presentFailure，别在管理端另立一套 */
const FAILURE_CODES = (['invalid_param', 'upstream_rate_limited', 'content_rejected'] as const).map(
  (code): { value: TaskErrorCode; label: string; tone: string } => {
    const p = presentFailure({ code, message: '', retryable: false, charged: false });
    return { value: code, label: p.title, tone: p.tone };
  },
);

const BREAKER_LABEL: Record<string, { text: string; tone: string }> = {
  closed: { text: '正常', tone: 'tone-success' },
  half_open: { text: '半开（试探中）', tone: 'tone-warning' },
  open: { text: '已熔断', tone: 'tone-danger' },
};

function errorText(err: unknown): string {
  return err instanceof ApiError ? err.message : '操作失败，请稍后重试';
}

export function AdminTasksPage() {
  const qc = useQueryClient();
  const [window, setWindow] = useState('24h');
  const [status, setStatus] = useState('');
  const [errorCode, setErrorCode] = useState('');

  const { data: stats } = useAdminTaskStats(window);
  const { data: tasks, isPending } = useAdminTasks(status, errorCode);
  const { data: breakers } = useAdminBreakers();

  function refresh(): void {
    void qc.invalidateQueries({ queryKey: ['admin', 'tasks'] });
    void qc.invalidateQueries({ queryKey: ['admin', 'breakers'] });
  }

  const retry = useMutation({
    mutationFn: (id: string) => adminApi.retryTask(id),
    onSuccess: () => {
      refresh();
      toast('已重新排队');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const cancel = useMutation({
    mutationFn: (id: string) => adminApi.cancelTask(id),
    onSuccess: () => {
      refresh();
      toast('已取消');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  const failureTotal = FAILURE_CODES.reduce((sum, c) => sum + (stats?.by_error_code[c.value] ?? 0), 0);

  return (
    <>
      <div className="admin-toolbar">
        <label className="sr-only" htmlFor="stat-window">
          统计窗口
        </label>
        <select
          id="stat-window"
          className="select inline-input"
          style={{ width: 'auto' }}
          value={window}
          onChange={(e) => setWindow(e.target.value)}
        >
          {WINDOWS.map((w) => (
            <option key={w.value} value={w.value}>
              {w.label}
            </option>
          ))}
        </select>
        <span className="spacer" />
        <button type="button" className="btn btn-sm" onClick={refresh}>
          刷新
        </button>
      </div>

      <div className="stat-grid">
        <div className="stat">
          <div className="label">排队中</div>
          <div className="value mono">{stats?.queued ?? 0}</div>
        </div>
        <div className="stat">
          <div className="label">生成中</div>
          <div className="value mono">{stats?.running ?? 0}</div>
        </div>
        <div className="stat tone-warning">
          <div className="label">最久等待</div>
          <div className="value mono">{formatDuration(stats?.oldest_queued_age_seconds)}</div>
        </div>
        <div className="stat tone-danger">
          <div className="label">失败</div>
          <div className="value mono">{failureTotal + (stats?.by_error_code.other ?? 0)}</div>
        </div>
      </div>

      <div className="admin-split">
        <div className="panel">
          <h3>失败分类</h3>
          {failureTotal === 0 ? (
            <p className="hint">该时间窗内没有失败任务。</p>
          ) : (
            <div className="bar-chart">
              {FAILURE_CODES.map((c) => {
                const count = stats?.by_error_code[c.value] ?? 0;
                return (
                  <div className="item" key={c.value}>
                    <div className="top">
                      <span>{c.label}</span>
                      <span className="mono">{count}</span>
                    </div>
                    <div className="bar">
                      <i
                        className={`tone-${c.tone}`}
                        style={{ width: `${Math.round((count / Math.max(failureTotal, 1)) * 100)}%` }}
                      />
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>

        <div className="panel">
          <h3>供应商熔断</h3>
          {breakers?.length ? (
            <div className="ledger">
              {breakers.map((b) => {
                const label = BREAKER_LABEL[b.state] ?? BREAKER_LABEL.closed;
                return (
                  <div className="row" key={b.provider_id}>
                    <span className="mono">{b.provider_id}</span>
                    <span className={`tag ${label.tone}`} style={{ marginLeft: 'auto' }}>
                      {label.text}
                    </span>
                    <span className="hint">{b.failure_count ?? 0} 次失败</span>
                  </div>
                );
              })}
            </div>
          ) : (
            <p className="hint">暂无供应商状态。</p>
          )}
        </div>
      </div>

      <div className="admin-toolbar" style={{ marginTop: 'var(--space-6)' }}>
        <label className="sr-only" htmlFor="task-status">
          任务状态
        </label>
        <select
          id="task-status"
          className="select inline-input"
          style={{ width: 'auto' }}
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          {STATUSES.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </select>
        <label className="sr-only" htmlFor="task-error">
          失败类型
        </label>
        <select
          id="task-error"
          className="select inline-input"
          style={{ width: 'auto' }}
          value={errorCode}
          onChange={(e) => setErrorCode(e.target.value)}
        >
          <option value="">全部失败类型</option>
          {FAILURE_CODES.map((c) => (
            <option key={c.value} value={c.value}>
              {c.label}
            </option>
          ))}
        </select>
        <span className="spacer" />
        <span className="hint">{tasks?.items.length ?? 0} 条</span>
      </div>

      {isPending ? (
        <p className="hint">加载中…</p>
      ) : tasks?.items.length ? (
        <table className="table">
          <thead>
            <tr>
              <th>任务</th>
              <th>用户</th>
              <th>模型</th>
              <th>状态</th>
              <th>失败原因</th>
              <th>提交</th>
              <th className="right">操作</th>
            </tr>
          </thead>
          <tbody>
            {tasks.items.map((t) => (
              <TaskRow
                key={t.id}
                task={t}
                busy={retry.isPending || cancel.isPending}
                onRetry={() => retry.mutate(t.id)}
                onCancel={() => cancel.mutate(t.id)}
              />
            ))}
          </tbody>
        </table>
      ) : (
        <div className="empty">
          <div className="title">没有匹配的任务</div>
          <p>换个筛选条件试试。</p>
        </div>
      )}
    </>
  );
}

function TaskRow({
  task,
  busy,
  onRetry,
  onCancel,
}: {
  task: AdminTask;
  busy: boolean;
  onRetry(): void;
  onCancel(): void;
}) {
  const failure = task.error ? presentFailure(task.error) : null;
  const retryable = task.status === 'failed' || task.status === 'canceled';
  const cancelable = task.status === 'queued' || task.status === 'running';

  return (
    <tr>
      <td className="strong">
        <div style={{ maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {task.prompt || '（无提示词）'}
        </div>
        <div className="hint mono" style={{ fontSize: 'var(--text-2xs)' }}>
          {task.id}
          {task.attempt && task.attempt > 1 ? ` · 第 ${task.attempt} 次` : ''}
        </div>
      </td>
      <td>{task.username ?? task.user_id ?? '—'}</td>
      <td className="mono">{task.model_id}</td>
      <td>
        <span className="tag">{task.status}</span>
      </td>
      <td>
        {failure ? (
          <>
            <span className={`tag tone-${failure.tone}`}>{failure.title}</span>
            <div className="hint" style={{ fontSize: 'var(--text-2xs)' }}>
              {task.error?.message}
            </div>
          </>
        ) : (
          '—'
        )}
      </td>
      <td>{formatRelative(task.created_at)}</td>
      <td>
        <div className="row-actions">
          {retryable && (
            <button type="button" className="btn btn-sm" onClick={onRetry} disabled={busy}>
              重试
            </button>
          )}
          {cancelable && (
            <button type="button" className="btn btn-sm btn-ghost" onClick={onCancel} disabled={busy}>
              取消
            </button>
          )}
        </div>
      </td>
    </tr>
  );
}
