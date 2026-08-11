import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { adminApi } from '@/api/endpoints';
import { qk, useAdminUserLedger, useAdminUsers } from '@/api/queries';
import type { AdminUser, CreditLedgerType, UserRole, UserStatus } from '@/api/types';
import { Sheet } from '@/components/admin/Sheet';
import { formatBytes, formatRelative } from '@/components/admin/format';
import { toast } from '@/stores/toast';
import { uuid } from '@/lib/uuid';

const LEDGER_LABEL: Record<CreditLedgerType, string> = {
  hold: '任务预扣',
  charge: '任务扣费',
  refund: '退回',
  topup: '发放',
  adjust: '调整',
};

const GB = 1024 ** 3;

function errorText(err: unknown): string {
  return err instanceof ApiError ? err.message : '操作失败，请稍后重试';
}

export function AdminUsersPage() {
  const qc = useQueryClient();
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  const { data: users, isPending } = useAdminUsers(query);

  const [editing, setEditing] = useState<AdminUser | null>(null);

  function refresh(): void {
    void qc.invalidateQueries({ queryKey: ['admin', 'users'] });
  }

  const patchUser = useMutation({
    mutationFn: ({ id, patch }: { id: string; patch: Parameters<typeof adminApi.updateUser>[1] }) =>
      adminApi.updateUser(id, patch),
    onSuccess: (user) => {
      refresh();
      setEditing((prev) => (prev && prev.id === user.id ? user : prev));
      toast('已保存');
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  return (
    <>
      <div className="admin-toolbar">
        <form
          style={{ display: 'flex', gap: 'var(--space-2)' }}
          onSubmit={(e) => {
            e.preventDefault();
            setQuery(search.trim());
          }}
        >
          <label className="sr-only" htmlFor="user-search">
            搜索用户名
          </label>
          <input
            id="user-search"
            className="input inline-input"
            placeholder="搜索用户名"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <button type="submit" className="btn btn-sm">
            搜索
          </button>
          {query && (
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              onClick={() => {
                setSearch('');
                setQuery('');
              }}
            >
              清除
            </button>
          )}
        </form>
        <span className="spacer" />
        <span className="hint">{users?.items.length ?? 0} 人</span>
      </div>

      {isPending ? (
        <p className="hint">加载中…</p>
      ) : users?.items.length ? (
        <table className="table">
          <thead>
            <tr>
              <th>用户</th>
              <th>角色</th>
              <th>状态</th>
              <th className="right">积分</th>
              <th className="right">存储</th>
              <th className="right">任务</th>
              <th>最近活跃</th>
              <th className="right">操作</th>
            </tr>
          </thead>
          <tbody>
            {users.items.map((u) => (
              <tr key={u.id}>
                <td className="strong">
                  {u.username}
                  <div className="hint mono" style={{ fontSize: 'var(--text-2xs)' }}>
                    {u.id}
                  </div>
                </td>
                <td>
                  {u.role === 'admin' ? (
                    <span className="tag tone-accent">管理员</span>
                  ) : (
                    <span className="tag">普通用户</span>
                  )}
                </td>
                <td>
                  {u.status === 'active' ? (
                    <span className="tag tone-success">正常</span>
                  ) : (
                    <span className="tag tone-danger">已停用</span>
                  )}
                </td>
                <td className="right mono">
                  {u.credits.toLocaleString()}
                  {u.credits_held > 0 && (
                    <div className="hint" style={{ fontSize: 'var(--text-2xs)' }}>
                      预扣 {u.credits_held.toLocaleString()}
                    </div>
                  )}
                </td>
                <td className="right mono">
                  {formatBytes(u.storage_used)} / {formatBytes(u.storage_quota)}
                </td>
                <td className="right mono">{u.task_count ?? 0}</td>
                <td>{formatRelative(u.last_active_at)}</td>
                <td>
                  <div className="row-actions">
                    <button type="button" className="btn btn-sm" onClick={() => setEditing(u)}>
                      管理
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <div className="empty">
          <div className="title">没有匹配的用户</div>
          <p>换个关键词试试。</p>
        </div>
      )}

      {editing && (
        <UserSheet
          key={editing.id}
          user={editing}
          busy={patchUser.isPending}
          onPatch={(patch) => patchUser.mutate({ id: editing.id, patch })}
          onCredited={() => {
            refresh();
            void qc.invalidateQueries({ queryKey: qk.adminUserLedger(editing.id) });
          }}
          onClose={() => setEditing(null)}
        />
      )}
    </>
  );
}

function UserSheet({
  user,
  busy,
  onPatch,
  onCredited,
  onClose,
}: {
  user: AdminUser;
  busy: boolean;
  onPatch(patch: { role?: UserRole; status?: UserStatus; storage_quota_bytes?: number }): void;
  onCredited(): void;
  onClose(): void;
}) {
  const { data: ledger } = useAdminUserLedger(user.id);
  const [quotaGb, setQuotaGb] = useState(() => String(Math.round((user.storage_quota / GB) * 10) / 10));
  const [amount, setAmount] = useState('');
  const [reason, setReason] = useState('');
  const [error, setError] = useState<string | null>(null);

  const adjust = useMutation({
    mutationFn: (payload: { amount: number; reason: string; idempotency_key: string }) =>
      adminApi.adjustCredits(user.id, payload),
    onSuccess: (entry) => {
      setAmount('');
      setReason('');
      onCredited();
      toast(`已调整 ${entry.amount > 0 ? '+' : '−'}${Math.abs(entry.amount)}，余额 ${entry.balance_after}`);
    },
    onError: (err) => setError(errorText(err)),
  });

  function submitAdjust(): void {
    const value = Number(amount);
    if (!amount.trim() || Number.isNaN(value) || value === 0) {
      setError('请填写非零的积分数量，负数表示扣减');
      return;
    }
    if (!reason.trim()) {
      setError('请填写原因，流水里要能查到是谁为什么发的');
      return;
    }
    setError(null);
    // 幂等键防止手抖点两次发两次积分；同一 key 后端会拒绝重放
    adjust.mutate({ amount: value, reason: reason.trim(), idempotency_key: uuid() });
  }

  return (
    <Sheet
      title={`用户 · ${user.username}`}
      onClose={onClose}
      footer={
        <button type="button" className="btn" onClick={onClose}>
          关闭
        </button>
      }
    >
      <div className="editor-section">
        <h4>账号</h4>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="u-role">角色</label>
            <select
              id="u-role"
              className="select"
              value={user.role}
              disabled={busy}
              onChange={(e) => onPatch({ role: e.target.value as UserRole })}
            >
              <option value="user">普通用户</option>
              <option value="admin">管理员</option>
            </select>
          </div>
          <div className="field">
            <label htmlFor="u-status">状态</label>
            <select
              id="u-status"
              className="select"
              value={user.status}
              disabled={busy}
              onChange={(e) => onPatch({ status: e.target.value as UserStatus })}
            >
              <option value="active">正常</option>
              <option value="disabled">停用</option>
            </select>
          </div>
          <div className="field span-2">
            <label htmlFor="u-quota">存储配额（GB）</label>
            <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
              <input
                id="u-quota"
                className="input"
                type="number"
                min={0}
                step={0.5}
                value={quotaGb}
                onChange={(e) => setQuotaGb(e.target.value)}
              />
              <button
                type="button"
                className="btn"
                disabled={busy}
                onClick={() => onPatch({ storage_quota_bytes: Math.round(Number(quotaGb) * GB) })}
              >
                应用
              </button>
            </div>
            <p className="hint">已用 {formatBytes(user.storage_used)}。</p>
          </div>
        </div>
      </div>

      <div className="editor-section">
        <h4>手工充值</h4>
        <div className="check-row">
          <span className="hint">
            当前余额 <span className="mono">{user.credits.toLocaleString()}</span>
            {user.credits_held > 0 && <>，其中 {user.credits_held.toLocaleString()} 被排队任务预扣</>}
          </span>
        </div>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="u-amount">积分数量</label>
            <input
              id="u-amount"
              className="input"
              type="number"
              placeholder="正数发放，负数扣减"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="u-reason">原因</label>
            <input
              id="u-reason"
              className="input"
              placeholder="如：活动补偿"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          </div>
        </div>
        {error && (
          <p className="hint danger" role="alert">
            {error}
          </p>
        )}
        <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
          {[100, 500, 1000].map((v) => (
            <button key={v} type="button" className="btn btn-sm btn-ghost" onClick={() => setAmount(String(v))}>
              +{v}
            </button>
          ))}
          <span className="spacer" style={{ flex: 1 }} />
          <button type="button" className="btn btn-primary" onClick={submitAdjust} disabled={adjust.isPending}>
            {adjust.isPending ? '提交中…' : '确认调整'}
          </button>
        </div>
      </div>

      <div className="editor-section">
        <h4>积分流水</h4>
        <div className="ledger">
          {(ledger?.items ?? []).map((entry) => (
            <div className="row" key={entry.id}>
              <span>{entry.reason ?? LEDGER_LABEL[entry.type]}</span>
              <span className="hint">{formatRelative(entry.created_at)}</span>
              <span className={`amt mono ${entry.amount < 0 ? 'minus' : 'plus'}`}>
                {entry.amount < 0 ? '−' : '+'}
                {Math.abs(entry.amount).toLocaleString()}
              </span>
            </div>
          ))}
          {!ledger?.items.length && <p className="hint">还没有积分流水。</p>}
        </div>
      </div>
    </Sheet>
  );
}
