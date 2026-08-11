import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';
import { adminApi } from '@/api/endpoints';
import { qk, useAdminStorage } from '@/api/queries';
import type { CleanupResult, CleanupTarget } from '@/api/types';
import { ConfirmDialog } from '@/components/ConfirmDialog';
import { formatBytes } from '@/components/admin/format';
import { toast } from '@/stores/toast';

// offline 的项后端一律返 400，页面上只做展示，不给触发入口。
const TARGETS: { value: CleanupTarget; label: string; desc: string; offline?: boolean }[] = [
  { value: 'expired_uploads', label: '过期上传', desc: '上传后一直没被任何任务使用的临时文件' },
  { value: 'soft_deleted_assets', label: '已删除资产', desc: '用户删除后进入回收站、超过保留期的产物' },
  {
    value: 'orphan_files',
    label: '孤儿文件（离线脚本）',
    desc: '数据库里已无引用、但仍占着对象存储的文件。要反向全量列举存储后端才能比对，只能由离线脚本执行，页面上无法触发。',
    offline: true,
  },
];

function errorText(err: unknown): string {
  return err instanceof ApiError ? err.message : '操作失败，请稍后重试';
}

export function AdminStoragePage() {
  const qc = useQueryClient();
  const { data: usage, isPending } = useAdminStorage();

  const [target, setTarget] = useState<CleanupTarget>('expired_uploads');
  const [days, setDays] = useState(30);
  const [preview, setPreview] = useState<CleanupResult | null>(null);
  const [confirming, setConfirming] = useState(false);

  const current = TARGETS.find((t) => t.value === target);
  const offline = current?.offline === true;

  const cleanup = useMutation({
    mutationFn: (dry_run: boolean) =>
      adminApi.storageCleanup({ dry_run, older_than_days: days, target }),
    onSuccess: (result) => {
      setPreview(result);
      if (!result.dry_run) {
        setConfirming(false);
        void qc.invalidateQueries({ queryKey: qk.adminStorage });
        toast(`已回收 ${formatBytes(result.reclaimed_bytes)}`);
      }
    },
    onError: (err) => toast(errorText(err), 'danger'),
  });

  return (
    <>
      <div className="stat-grid">
        <div className="stat">
          <div className="label">已用总量</div>
          <div className="value mono">{formatBytes(usage?.total_bytes ?? 0)}</div>
        </div>
        <div className="stat">
          <div className="label">产物数</div>
          <div className="value mono">{(usage?.asset_count ?? 0).toLocaleString()}</div>
        </div>
        <div className="stat tone-warning">
          <div className="label">待清理上传</div>
          <div className="value mono">{formatBytes(usage?.upload_pending_bytes ?? 0)}</div>
        </div>
        <div className="stat tone-muted">
          <div className="label">回收站</div>
          <div className="value mono">{formatBytes(usage?.soft_deleted_bytes ?? 0)}</div>
        </div>
      </div>

      <div className="admin-split">
        <div className="panel">
          <h3>用户占用</h3>
          {isPending ? (
            <p className="hint">加载中…</p>
          ) : usage?.top_users?.length ? (
            <div className="bar-chart">
              {usage.top_users.map((u) => {
                const ratio = Math.min(1, u.used_bytes / Math.max(u.quota_bytes, 1));
                const over = ratio >= 0.9;
                return (
                  <div className="item" key={u.user_id}>
                    <div className="top">
                      <span>{u.username}</span>
                      <span className="mono">
                        {formatBytes(u.used_bytes)} / {formatBytes(u.quota_bytes)}
                      </span>
                    </div>
                    <div className="bar">
                      <i className={over ? 'tone-danger' : ''} style={{ width: `${Math.round(ratio * 100)}%` }} />
                    </div>
                  </div>
                );
              })}
            </div>
          ) : (
            <p className="hint">还没有用户占用数据。</p>
          )}
        </div>

        <div className="panel">
          <h3>清理</h3>
          <div className="field">
            <label htmlFor="cl-target">清理对象</label>
            <select
              id="cl-target"
              className="select"
              value={target}
              onChange={(e) => {
                setTarget(e.target.value as CleanupTarget);
                setPreview(null);
              }}
            >
              {TARGETS.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
            <p className="hint">{current?.desc}</p>
          </div>
          <div className="field">
            <label htmlFor="cl-days">早于多少天</label>
            <input
              id="cl-days"
              className="input"
              type="number"
              min={1}
              value={days}
              onChange={(e) => {
                setDays(Number(e.target.value));
                setPreview(null);
              }}
            />
          </div>

          <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
            <button
              type="button"
              className="btn"
              disabled={offline || cleanup.isPending}
              onClick={() => cleanup.mutate(true)}
            >
              预览
            </button>
            {/* 删除不可逆，必须先看过预览再放行 */}
            <button
              type="button"
              className="btn btn-primary"
              disabled={
                offline || cleanup.isPending || !preview?.dry_run || preview.candidate_count === 0
              }
              onClick={() => setConfirming(true)}
            >
              执行清理
            </button>
          </div>

          {preview && (
            <div style={{ marginTop: 'var(--space-4)' }}>
              <div className="kv">
                <span className="k">{preview.dry_run ? '待清理' : '已清理'}</span>
                <span className="v mono">{preview.candidate_count} 项</span>
              </div>
              <div className="kv">
                <span className="k">{preview.dry_run ? '可回收' : '已回收'}</span>
                <span className="v mono">{formatBytes(preview.reclaimed_bytes)}</span>
              </div>
              {preview.samples?.length ? (
                <ul className="hint mono" style={{ margin: 'var(--space-2) 0 0', paddingLeft: '1.2em' }}>
                  {preview.samples.map((s) => (
                    <li key={s}>{s}</li>
                  ))}
                </ul>
              ) : null}
            </div>
          )}
        </div>
      </div>

      {confirming && preview && (
        <ConfirmDialog
          title="执行清理"
          body={`确认清理 ${preview.candidate_count} 项？该操作不可撤销。`}
          confirmLabel="执行清理"
          busyLabel="清理中…"
          danger
          busy={cleanup.isPending}
          onCancel={() => setConfirming(false)}
          onConfirm={() => cleanup.mutate(false)}
        />
      )}
    </>
  );
}
