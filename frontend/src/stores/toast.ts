import { create } from 'zustand';

export interface Toast {
  id: number;
  text: string;
  tone: 'default' | 'danger';
}

interface ToastState {
  toasts: Toast[];
  push(text: string, tone?: Toast['tone']): void;
  dismiss(id: number): void;
}

let seq = 0;

export const useToastStore = create<ToastState>((set) => ({
  toasts: [],
  push(text, tone = 'default') {
    const id = ++seq;
    set((s) => ({ toasts: [...s.toasts, { id, text, tone }] }));
    window.setTimeout(() => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })), 4000);
  },
  dismiss(id) {
    set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) }));
  },
}));

export const toast = (text: string, tone?: Toast['tone']) => useToastStore.getState().push(text, tone);
