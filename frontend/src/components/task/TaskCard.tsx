import type { Task } from '@/api/types';
import { AssetTile } from '../AssetTile';
import { displayProgress, presentFailure, progressCopy } from './failure';

interface TaskCardProps {
  task: Task;
  onCancel(task: Task): void;
  onRetry(task: Task): void;
  onEditPrompt(task: Task): void;
  onDismiss(task: Task): void;
}

export function TaskCard({ task, onCancel, onRetry, onEditPrompt, onDismiss }: TaskCardProps) {
  if (task.status === 'succeeded') {
    return (
      <>
        {(task.assets ?? []).map((asset) => (
          <AssetTile key={asset.id} asset={asset} />
        ))}
      </>
    );
  }

  if (task.local === 'submitting') {
    return (
      <div className="task-card" aria-live="polite">
        <div className="skeleton" />
        <div className="task-state" style={{ background: 'transparent' }}>
          <div className="label">提交中</div>
        </div>
      </div>
    );
  }

  if (task.status === 'queued') {
    return (
      <div className="task-card" aria-live="polite">
        <div className="task-state">
          <div className="err-icon err-muted" aria-hidden="true">
            ⏳
          </div>
          <div className="label">
            排队中{task.queue_position !== null ? ` · 第 ${task.queue_position} 位` : ''}
          </div>
          <button type="button" className="btn btn-sm" onClick={() => onCancel(task)}>
            取消
          </button>
        </div>
      </div>
    );
  }

  if (task.status === 'running') {
    return (
      <div className="task-card" aria-live="polite">
        <div className="skeleton" />
        <div className="task-state" style={{ background: 'transparent' }}>
          <div className="label">生成中</div>
          <div className="progress">
            <i style={{ width: `${Math.round(displayProgress(task) * 100)}%` }} />
          </div>
          <div className="sub mono">{progressCopy(task)}</div>
        </div>
      </div>
    );
  }

  if (task.status === 'canceled') {
    return (
      <div className="task-card">
        <div className="task-state">
          <div className="err-icon err-muted" aria-hidden="true">
            ⊘
          </div>
          <div className="label">已取消</div>
          <button type="button" className="btn btn-sm" onClick={() => onDismiss(task)}>
            移除
          </button>
        </div>
      </div>
    );
  }

  const f = presentFailure(task.error);
  const retry = task.error?.retry;

  return (
    <div className="task-card failed" role="alert">
      <div className="task-state">
        <div className={`err-icon err-${f.tone}`} aria-hidden="true">
          {f.icon}
        </div>
        <div className="label" style={{ color: `var(--${f.tone === 'muted' ? 'muted' : f.tone})` }}>
          {f.title}
        </div>
        <div className="sub">{task.error?.message}</div>

        {retry && (
          <>
            <div className="sub">
              正在自动重试 ({retry.attempt}/{retry.max_attempts})
            </div>
            <div className="progress" style={{ width: '50%' }}>
              <i style={{ width: `${(retry.attempt / retry.max_attempts) * 100}%`, background: 'var(--warning)' }} />
            </div>
          </>
        )}

        {f.action === 'retry' && (
          <button type="button" className="btn btn-sm" onClick={() => onRetry(task)}>
            {f.actionLabel}
          </button>
        )}
        {f.action === 'edit_prompt' && (
          <button type="button" className="btn btn-sm" onClick={() => onEditPrompt(task)}>
            {f.actionLabel}
          </button>
        )}
      </div>
      {/* 三类失败都不扣费，但仍显式读 error.charged 而不是写死 */}
      {task.error && !task.error.charged && <span className="charged-tag">未扣费</span>}
    </div>
  );
}
