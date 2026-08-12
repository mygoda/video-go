import { useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useCreditLedger, useMe, useModels, useTasks, useTrashedProjects } from '@/api/queries';
import { qk } from '@/api/queries';
import type { CreditLedgerEntry, Task } from '@/api/types';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
import { formatBytes } from '@/components/admin/format';
import { useAuthStore } from '@/stores/auth';
import { toast } from '@/stores/toast';

const MODALITY_LABEL: Record<Task['modality'], string> = {
  image: '图片生成',
  video: '视频生成',
  text: '文本生成',
};

/**
 * 把账本聚成用户看得懂的消耗记录：同一任务的 hold（预扣）/ charge（结算）/ refund
 * （退回）合并成一条，展示净额 + 是哪一步哪个模型；取消 / 失败的直接显示 refund 的
 * 原因文案。管理员发放 / 调整各自一条。纯内部记账（无 task 的 hold/charge）不外显。
 */
interface LedgerRow {
  key: string;
  label: string;
  amount: number;
  at: string;
}

function buildLedgerRows(
  entries: CreditLedgerEntry[],
  taskById: Map<string, Task>,
  modelNameById: Map<string, string>,
): LedgerRow[] {
  const rows: LedgerRow[] = [];
  const byTask = new Map<string, CreditLedgerEntry[]>();
  for (const e of entries) {
    if (e.type === 'topup') {
      rows.push({ key: e.id, label: '管理员发放', amount: e.amount, at: e.created_at });
      continue;
    }
    if (e.type === 'adjust') {
      rows.push({ key: e.id, label: `管理员调整${e.reason ? ` · ${e.reason}` : ''}`, amount: e.amount, at: e.created_at });
      continue;
    }
    if (!e.task_id) continue; // 无任务的 hold/charge 是内部记账，不外显
    const arr = byTask.get(e.task_id) ?? [];
    arr.push(e);
    byTask.set(e.task_id, arr);
  }
  for (const [taskId, arr] of byTask) {
    const net = arr.reduce((s, e) => s + e.amount, 0);
    const refund = arr.find((e) => e.type === 'refund');
    // 净额 0 又没退回 = 免费任务（如合成），没有消耗信息，不占版面
    if (net === 0 && !refund) continue;
    const at = arr.reduce((m, e) => (e.created_at > m ? e.created_at : m), arr[0].created_at);
    const task = taskById.get(taskId);
    const modality = task ? MODALITY_LABEL[task.modality] : '任务';
    const model = task ? modelNameById.get(task.model_id) : undefined;
    let label = `${modality}${model ? ` · ${model}` : ''}`;
    if (refund) {
      // refund.reason 已是人话（"任务已取消" / 失败原因），直接显示
      label += ` · ${refund.reason?.trim() || '已退回'}`;
    } else {
      const count = task?.assets?.length ?? 0;
      if (count > 1) label += ` ×${count}`;
    }
    rows.push({ key: taskId, label, amount: net, at });
  }
  rows.sort((a, b) => (a.at < b.at ? 1 : -1));
  return rows;
}

export function ProfilePage() {
  const isAuthed = useAuthStore((s) => s.isAuthed);
  const signOut = useAuthStore((s) => s.signOut);
  const { data: me } = useMe(isAuthed);
  const { data: ledger } = useCreditLedger();
  const { data: tasks } = useTasks();
  const { data: imageModels } = useModels('image');
  const { data: videoModels } = useModels('video');
  // 文本目录也得拉：画布的对话 / 拆分镜 / 优化都开始扣费了，账本里现在有文本
  // 任务，少这一档它们的模型名全查不到，条目就秃成一句「文本生成」。
  const { data: textModels } = useModels('text');
  const navigate = useNavigate();
  const qc = useQueryClient();

  const { data: trashed } = useTrashedProjects();
  const restore = useMutation({
    mutationFn: (id: string) => api.restoreProject(id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.trashedProjects });
      void qc.invalidateQueries({ queryKey: qk.projects });
      toast('已恢复');
    },
    onError: () => toast('恢复失败，请重试'),
  });

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

  function logout(): void {
    signOut();
    qc.clear();
    navigate('/login', { replace: true });
  }

  const usedRatio = me ? Math.min(1, me.storage_used / Math.max(me.storage_quota, 1)) : 0;

  const taskById = new Map((tasks ?? []).map((t) => [t.id, t]));
  const modelNameById = new Map(
    [...(imageModels ?? []), ...(videoModels ?? []), ...(textModels ?? [])].map((m) => [m.id, m.name]),
  );
  // 按任务聚合成一条条净消耗，取消 / 失败带原因；管理员发放 / 调整各自一条。
  const ledgerRows = buildLedgerRows(ledger?.items ?? [], taskById, modelNameById);

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
              {ledgerRows.map((row) => (
                <div className="row" key={row.key}>
                  <span>{row.label}</span>
                  <span className={`amt mono ${row.amount < 0 ? 'minus' : 'plus'}`}>
                    {row.amount < 0 ? '−' : '+'}
                    {Math.abs(row.amount).toLocaleString()}
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
            <button type="button" className="btn" style={{ width: '100%' }} onClick={logout}>
              退出登录
            </button>
          </div>
        </div>
      </div>

      <div className="panel" style={{ marginTop: 'var(--space-5)' }}>
        <h3>回收站</h3>
        {trashed && trashed.length > 0 ? (
          <div className="trash-list">
            {trashed.map((p) => (
              <div className="trash-row" key={p.id}>
                <span className="trash-name">{p.name || '未命名短剧'}</span>
                <span className="hint mono">{p.card_count} 卡</span>
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => restore.mutate(p.id)}
                  disabled={restore.isPending}
                >
                  恢复
                </button>
              </div>
            ))}
          </div>
        ) : (
          <p className="hint">回收站是空的。删除的短剧会移到这里，其资源随之隐藏，可随时恢复。</p>
        )}
      </div>
    </main>
  );
}
