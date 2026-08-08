import { useToastStore } from '@/stores/toast';

export function ToastStack() {
  const toasts = useToastStore((s) => s.toasts);
  const dismiss = useToastStore((s) => s.dismiss);
  if (!toasts.length) return null;

  return (
    <div className="toast-stack" role="status" aria-live="polite">
      {toasts.map((t) => (
        <button
          key={t.id}
          type="button"
          className={`toast${t.tone === 'danger' ? ' danger' : ''}`}
          onClick={() => dismiss(t.id)}
        >
          {t.text}
        </button>
      ))}
    </div>
  );
}
