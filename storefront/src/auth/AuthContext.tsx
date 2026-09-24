// The whole app's own session state: a bearer token in localStorage
// (this is a session token exactly like the one C6's own
// retail_sessions table issues -- never a password, never an API key),
// restored on load and validated once against GET /v1/retail/me so a
// stale/revoked token in localStorage is discovered immediately rather
// than trusted until the first real action fails.
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { api, ApiError } from "../api/client";

const STORAGE_KEY = "storefront_session";

interface StoredSession {
  token: string;
  retailCustomerId: number;
  email: string;
}

interface AuthContextValue {
  session: StoredSession | null;
  loading: boolean;
  register: (email: string, password: string) => Promise<void>;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

function loadStored(): { token: string } | null {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as { token: string };
  } catch {
    return null;
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<StoredSession | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const stored = loadStored();
    if (!stored) {
      setLoading(false);
      return;
    }
    api
      .me(stored.token)
      .then((me) => setSession({ token: stored.token, retailCustomerId: me.retail_customer_id, email: me.email }))
      .catch(() => {
        // Stale, expired, or revoked -- discovered now, not on the
        // first real action a user takes.
        localStorage.removeItem(STORAGE_KEY);
        setSession(null);
      })
      .finally(() => setLoading(false));
  }, []);

  const persist = useCallback((token: string, retailCustomerId: number, email: string) => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ token }));
    setSession({ token, retailCustomerId, email });
  }, []);

  const register = useCallback(
    async (email: string, password: string) => {
      const resp = await api.register(email, password);
      persist(resp.session_token, resp.retail_customer_id, email);
    },
    [persist],
  );

  const login = useCallback(
    async (email: string, password: string) => {
      const resp = await api.login(email, password);
      persist(resp.session_token, resp.retail_customer_id, email);
    },
    [persist],
  );

  const logout = useCallback(async () => {
    if (session) {
      // Best-effort -- an already-invalid token on the server should
      // still clear local state, matching the backend's own
      // idempotent-revoke posture.
      try {
        await api.logout(session.token);
      } catch (err) {
        if (!(err instanceof ApiError)) throw err;
      }
    }
    localStorage.removeItem(STORAGE_KEY);
    setSession(null);
  }, [session]);

  const value = useMemo(() => ({ session, loading, register, login, logout }), [session, loading, register, login, logout]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth called outside AuthProvider");
  return ctx;
}
