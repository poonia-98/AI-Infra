/**
 * Auth token management.
 * Tokens are kept in memory (not localStorage) to prevent XSS theft.
 * Refresh token is stored as HttpOnly cookie managed by the API server.
 */

const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8000';

interface TokenStore {
  accessToken: string | null;
  expiresAt: number | null;
}

const store: TokenStore = { accessToken: null, expiresAt: null };

let refreshTimer: ReturnType<typeof setTimeout> | null = null;

export function setAccessToken(token: string, expiresInMs: number) {
  store.accessToken = token;
  store.expiresAt = Date.now() + expiresInMs;
  scheduleRefresh(expiresInMs);
}

export function getAccessToken(): string | null {
  if (!store.accessToken) return null;
  if (store.expiresAt && Date.now() > store.expiresAt - 30_000) return null; // expired / near expiry
  return store.accessToken;
}

export function clearTokens() {
  store.accessToken = null;
  store.expiresAt = null;
  if (refreshTimer) clearTimeout(refreshTimer);
}

function scheduleRefresh(expiresInMs: number) {
  if (refreshTimer) clearTimeout(refreshTimer);
  const refreshIn = Math.max(expiresInMs - 60_000, 10_000); // 1 min before expiry
  refreshTimer = setTimeout(async () => {
    try { await silentRefresh(); } catch { /* will be caught at request time */ }
  }, refreshIn);
}

export async function silentRefresh(): Promise<boolean> {
  try {
    const res = await fetch(`${API_URL}/api/v1/auth/refresh`, {
      method: 'POST',
      credentials: 'include', // sends httpOnly refresh cookie
      headers: { 'Content-Type': 'application/json' },
    });
    if (!res.ok) { clearTokens(); return false; }
    const data = await res.json();
    if (data.access_token) {
      setAccessToken(data.access_token, (data.expires_in ?? 3600) * 1000);
      return true;
    }
    return false;
  } catch {
    return false;
  }
}

export interface LoginResult {
  success: boolean;
  error?: string;
  user?: UserProfile;
}

export interface UserProfile {
  user_id: string;
  email: string;
  name: string;
  role: string;
  organisation_id: string;
  organisation_name: string;
  avatar_url?: string;
}

export async function login(email: string, password: string): Promise<LoginResult> {
  try {
    const res = await fetch(`${API_URL}/api/v1/auth/login`, {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password }),
    });

    if (res.status === 401) return { success: false, error: 'Invalid credentials' };
    if (res.status === 429) return { success: false, error: 'Too many attempts. Try again later.' };
    if (res.status === 403) return { success: false, error: 'Account locked or access denied' };
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      return { success: false, error: data.detail || 'Authentication failed' };
    }

    const data = await res.json();
    setAccessToken(data.access_token, (data.expires_in ?? 3600) * 1000);

    return {
      success: true,
      user: {
        user_id: data.user_id,
        email: data.email,
        name: data.name,
        role: data.role,
        organisation_id: data.organisation_id,
        organisation_name: data.organisation_name,
        avatar_url: data.avatar_url,
      },
    };
  } catch (e) {
    return { success: false, error: 'Network error. Check connection.' };
  }
}

export async function logout(): Promise<void> {
  try {
    await fetch(`${API_URL}/api/v1/auth/logout`, {
      method: 'POST',
      credentials: 'include',
      headers: getAuthHeaders(),
    });
  } catch { /* best-effort */ }
  clearTokens();
}

export async function getMe(): Promise<UserProfile | null> {
  const token = getAccessToken();
  if (!token) {
    // Try silent refresh first
    const ok = await silentRefresh();
    if (!ok) return null;
  }
  try {
    const res = await fetch(`${API_URL}/api/v1/auth/me`, {
      credentials: 'include',
      headers: getAuthHeaders(),
    });
    if (!res.ok) return null;
    return res.json();
  } catch { return null; }
}

export function getAuthHeaders(): Record<string, string> {
  const token = getAccessToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

export function isAuthenticated(): boolean {
  return getAccessToken() !== null;
}