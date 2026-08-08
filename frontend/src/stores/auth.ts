import { create } from 'zustand';
import { getToken, onTokenChange, setToken } from '@/api/token';

interface AuthState {
  token: string | null;
  isAuthed: boolean;
  signIn(token: string): void;
  signOut(): void;
}

export const useAuthStore = create<AuthState>((set) => ({
  token: getToken(),
  isAuthed: Boolean(getToken()),
  signIn(token) {
    setToken(token);
    set({ token, isAuthed: true });
  },
  signOut() {
    setToken(null);
    set({ token: null, isAuthed: false });
  },
}));

// 另一个标签页登出、或 401 拦截器清 token 时，本页要跟着掉线
onTokenChange((token) => {
  useAuthStore.setState({ token, isAuthed: Boolean(token) });
});
