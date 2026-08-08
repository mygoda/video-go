import type { Asset, Task, TaskError } from '@/api/types';

export interface FailurePresentation {
  tone: 'danger' | 'warning' | 'muted';
  icon: string;
  title: string;
  action: 'retry' | 'edit_prompt' | 'credits' | 'none';
  actionLabel?: string;
}

/**
 * 失败三类的呈现规则（frontend-design.md §4.5）。严格按 code 分支，不解析 message ——
 * 后端换文案不该让前端的行为变。
 */
export function presentFailure(error: TaskError | null): FailurePresentation {
  switch (error?.code) {
    case 'invalid_param':
      return { tone: 'danger', icon: '!', title: '参数不合法', action: 'retry', actionLabel: '修正并重试' };
    case 'upstream_rate_limited':
      return { tone: 'warning', icon: '↻', title: '上游繁忙', action: 'none' };
    case 'content_rejected':
      return { tone: 'muted', icon: '⃠', title: '内容审核未通过', action: 'edit_prompt', actionLabel: '修改提示词' };
    case 'insufficient_credit':
      return { tone: 'danger', icon: '⚡', title: '积分不足', action: 'credits', actionLabel: '查看积分' };
    default:
      return { tone: 'danger', icon: '!', title: '生成失败', action: 'retry', actionLabel: '重试' };
  }
}

/** 乐观进度：真值缺失时按 eta 推，且死卡 90% —— 到 100% 却还没好比慢更伤 */
export function displayProgress(task: Task): number {
  return Math.min(task.progress ?? 0, 0.9);
}

export function progressCopy(task: Task): string {
  if (task.progress !== null && task.progress >= 0.9) return '即将完成';
  if (task.eta_seconds === null) return '生成中';
  return `约 ${task.eta_seconds}s`;
}

export function taskAssets(task: Task): Asset[] {
  return task.status === 'succeeded' ? task.assets : [];
}
