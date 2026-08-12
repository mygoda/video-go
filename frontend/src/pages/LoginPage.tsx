import { useState, type FormEvent } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { api } from '@/api/endpoints';
import { ApiError } from '@/api/client';
import { useAuthStore } from '@/stores/auth';

type Mode = 'login' | 'register';

export function LoginPage() {
  const [mode, setMode] = useState<Mode>('login');
  const [username, setUsername] = useState('yuchen');
  const [password, setPassword] = useState('password');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const signIn = useAuthStore((s) => s.signIn);
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const next = params.get('next') || '/create/image';

  async function submit(e: FormEvent): Promise<void> {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const res = mode === 'login' ? await api.login(username, password) : await api.register(username, password);
      signIn(res.token);
      navigate(next, { replace: true });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络异常，请稍后重试');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="auth-screen">
      <form className="auth" onSubmit={submit}>
        <div className="auth-logo">
          <span className="logo-mark" style={{ width: 28, height: 28, fontSize: 'var(--text-sm)' }}>
            D
          </span>
          <span style={{ fontSize: 'var(--text-md)', fontWeight: 'var(--weight-semi)' }}>dreamVideo</span>
        </div>

        <h1>{mode === 'login' ? '开始创作' : '创建账号'}</h1>
        <p className="lead">一个账号，接入全部图像与视频模型</p>

        <div className="field">
          <label htmlFor="username">账号</label>
          <input
            className="input"
            id="username"
            name="username"
            autoComplete="username"
            placeholder="用户名"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
          />
        </div>
        <div className="field">
          <label htmlFor="password">密码</label>
          <input
            className="input"
            id="password"
            name="password"
            type="password"
            autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
            placeholder="••••••••"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </div>

        {/* 错误贴在表单里，不用 toast：登录失败要留在原地可读可改 */}
        {error && (
          <p className="hint danger" style={{ margin: '-8px 0 16px' }} role="alert">
            {error}
          </p>
        )}

        <button className="btn btn-primary btn-lg" style={{ width: '100%' }} type="submit" disabled={busy}>
          {busy ? '请稍候…' : mode === 'login' ? '登录' : '注册'}
        </button>

        <p className="switch">
          {mode === 'login' ? '还没有账号？' : '已经有账号了？'}
          <button
            type="button"
            onClick={() => {
              setMode(mode === 'login' ? 'register' : 'login');
              setError(null);
            }}
          >
            {mode === 'login' ? '注册' : '登录'}
          </button>
        </p>
      </form>
    </div>
  );
}
