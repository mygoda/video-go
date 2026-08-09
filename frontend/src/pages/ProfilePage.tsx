import { useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { useCreditLedger, useMe, useModels, useTasks } from '@/api/queries';
import type { CreditLedgerEntry, Task, TaskErrorCode } from '@/api/types';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
import { formatBytes } from '@/components/admin/format';
import { presentFailure } from '@/components/task/failure';
import { useAuthStore } from '@/stores/auth';
import { toast } from '@/stores/toast';

const MODALITY_LABEL: Record<Task['modality'], string> = {
  image: '图片生成',
  video: '视频生成',
  text: '文本生成',
};

/** 只有这几个 code 有专门文案，其余走不带后缀的「任务失败退回」，免得出现「退回 · 生成失败」这种绕口话 */
const NAMED_ERROR_CODES = new Set<string>([
  'invalid_param',
  'upstream_rate_limited',
  'content_rejected',
  'insufficient_credit',
]);

/**
 * 把一条账本记录说成人话。hold / charge 的 reason 是内部记账串（"task hold"），
 * 一律不外显；模态与模型名靠 task_id 回查任务拿，查不到就退回泛化文案。
 */
function describeEntry(
  entry: CreditLedgerEntry,
  taskById: Map<string, Task>,
  modelNameById: Map<string, string>,
): string {
  if (entry.type === 'topup') return '管理员发放';
  if (entry.type === 'adjust') return '管理员调整';
  if (entry.type === 'refund') {
    const code = entry.reason?.split(/[:：]/).pop()?.trim() ?? '';
    if (!NAMED_ERROR_CODES.has(code)) return '任务失败退回';
    const p = presentFailure({ code: code as TaskErrorCode, message: '', retryable: false, charged: false });
    return `任务失败退回 · ${p.title}`;
  }

  const task = entry.task_id ? taskById.get(entry.task_id) : undefined;
  if (!task) return '任务消耗';
  const parts = [MODALITY_LABEL[task.modality]];
  const modelName = modelNameById.get(task.model_id);
  if (modelName) parts.push(modelName);
  const count = task.assets?.length ?? 0;
  return `${parts.join(' · ')}${count > 1 ? ` ×${count}` : ''}`;
}

export function ProfilePage() {
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const signOut = useAuthStore((s) => s.signOut);
  const { data: me } = useMe(isAuthed);
  const { data: ledger } = useCreditLedger();
  const { data: tasks } = useTasks();
  const { data: imageModels } = useModels('image');
  const { data: videoModels } = useModels('video');
  const navigate = useNavigate();
  const qc = useQueryClient();

  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function changePassword(e: FormEvent): Promise<void> {
    e.preventDefault();
    setError(null);
    if (next.length < 8) {
      setError('新密码至少 8 位');
      return;
    }
    setBusy(true);
    try {
      await api.changePassword(current, next);
      setCurrent('');
      setNext('');
      toast('密码已更新');
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '修改失败，请稍后重试');
    } finally {
      setBusy(false);
    }
  }

  async function logout(): Promise<void> {
    try {
      await api.logout();
    } finally {
      signOut();
      qc.clear();
      navigate('/login', { replace: true });
    }
  }

  const usedRatio = me ? Math.min(1, me.storage_used / Math.max(me.storage_quota, 1)) : 0;

  const taskById = new Map((tasks ?? []).map((t) => [t.id, t]));
  const modelNameById = new Map(
    [...(imageModels ?? []), ...(videoModels ?? [])].map((m) => [m.id, m.name]),
  );
  // hold/charge 是同一笔消费的两条记账，charge 结算为 0 时只是记账收尾，对用户没有信息量
  const ledgerRows = (ledger?.items ?? []).filter((e) => !(e.type === 'charge' && e.amount === 0));

  return (
    <main className="page" style={{ maxWidth: 900 }}>
      <h1 className="page-title">个人中心</h1>
      <p className="page-sub">
        {me?.username ?? '—'} · 注册于 {me ? me.created_at.slice(0, 10) : '—'}
      </p>

      <div className="cols">
        <div className="stack">
          <div className="panel">
            <h3>积分</h3>
            <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, marginBottom: 'var(--space-5)' }}>
              <span className="big-num mono" style={{ color: 'var(--credit)' }}>
                {(me?.credits ?? 0).toLocaleString()}
              </span>
              <span className="hint">可用积分</span>
            </div>
            {Boolean(me?.credits_held) && (
              <p className="hint" style={{ marginTop: 'calc(-1 * var(--space-4))', marginBottom: 'var(--space-5)' }}>
                另有 <span className="mono">{(me?.credits_held ?? 0).toLocaleString()}</span> 被排队中的任务预扣。
              </p>
            )}
            {/* 平台统一持有 Key + 积分记账，不接支付，所以没有充值按钮 */}
            <p className="hint">积分由管理员发放，本版本不接支付。</p>
          </div>

          <div className="panel">
            <h3>最近消耗</h3>
            <div className="ledger">
              {ledgerRows.map((entry) => (
                <div className="row" key={entry.id}>
                  <span>{describeEntry(entry, taskById, modelNameById)}</span>
                  <span className={`amt mono ${entry.amount < 0 ? 'minus' : 'plus'}`}>
                    {entry.amount < 0 ? '−' : '+'}
                    {Math.abs(entry.amount).toLocaleString()}
                  </span>
                </div>
              ))}
              {ledgerRows.length === 0 && <p className="hint">还没有积分流水。</p>}
            </div>
          </div>
        </div>

        <div className="stack">
          <div className="panel">
            <h3>存储</h3>
            <div className="bar" style={{ marginBottom: 10 }}>
              <i style={{ width: `${Math.round(usedRatio * 100)}%` }} />
            </div>
            <div className="kv" style={{ paddingTop: 0 }}>
              <span className="k">已用</span>
              <span className="v mono">{formatBytes(me?.storage_used ?? 0)}</span>
            </div>
            <div className="kv">
              <span className="k">配额</span>
              <span className="v mono">{formatBytes(me?.storage_quota ?? 0)}</span>
            </div>
          </div>

          <form className="panel" onSubmit={changePassword}>
            <h3>修改密码</h3>
            <div className="field">
              <label htmlFor="current-password">当前密码</label>
              <input
                className="input"
                id="current-password"
                type="password"
                autoComplete="current-password"
                placeholder="••••••••"
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
                required
              />
            </div>
            <div className="field">
              <label htmlFor="new-password">新密码</label>
              <input
                className="input"
                id="new-password"
                type="password"
                autoComplete="new-password"
                placeholder="至少 8 位"
                value={next}
                onChange={(e) => setNext(e.target.value)}
                required
              />
            </div>
            {error && (
              <p className="hint danger" role="alert" style={{ marginTop: -8, marginBottom: 12 }}>
                {error}
              </p>
            )}
            <button className="btn" type="submit" disabled={busy}>
              {busy ? '保存中…' : '保存'}
            </button>
          </form>

          <div className="panel">
            <h3>账号</h3>
            <button type="button" className="btn" style={{ width: '100%' }} onClick={() => void logout()}>
              退出登录
            </button>
          </div>
        </div>
      </div>
    </main>
  );
}
