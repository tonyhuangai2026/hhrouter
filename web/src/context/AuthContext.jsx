import React, {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  useMemo,
} from 'react';
import {
  login as apiLogin,
  register as apiRegister,
  getSelf as apiGetSelf,
} from '../api/auth';
import { getToken, setToken } from '../api/client';

const AuthContext = createContext(null);

const USER_STORAGE_KEY = 'arp_user';

function readStoredUser() {
  try {
    const raw = localStorage.getItem(USER_STORAGE_KEY);
    return raw ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
}

function persistUser(user) {
  if (user) {
    localStorage.setItem(USER_STORAGE_KEY, JSON.stringify(user));
  } else {
    localStorage.removeItem(USER_STORAGE_KEY);
  }
}

// Normalize the various places a backend might put the token / user.
function extractAuth(data) {
  const token = data.token || data.jwt || data.accessToken || null;
  const user = data.user || data.data || null;
  return { token, user };
}

export function AuthProvider({ children }) {
  const [user, setUser] = useState(() => readStoredUser());
  const [token, setTokenState] = useState(() => getToken());
  const [loading, setLoading] = useState(true);

  // On mount, if we have a token but no user (or want fresh data), fetch self.
  useEffect(() => {
    let active = true;
    async function bootstrap() {
      if (!getToken()) {
        setLoading(false);
        return;
      }
      try {
        const self = await apiGetSelf();
        if (active) {
          const resolved = self.user || self.data || self;
          setUser(resolved);
          persistUser(resolved);
        }
      } catch {
        // 401 handling lives in the axios interceptor; here we just stop loading.
      } finally {
        if (active) setLoading(false);
      }
    }
    bootstrap();
    return () => {
      active = false;
    };
  }, []);

  const applyAuth = useCallback((data) => {
    const { token: t, user: u } = extractAuth(data);
    if (t) {
      setToken(t);
      setTokenState(t);
    }
    if (u) {
      setUser(u);
      persistUser(u);
    }
    return { token: t, user: u };
  }, []);

  const login = useCallback(
    async (credentials) => {
      const data = await apiLogin(credentials);
      return applyAuth(data);
    },
    [applyAuth]
  );

  const register = useCallback(
    async (payload) => {
      const data = await apiRegister(payload);
      return applyAuth(data);
    },
    [applyAuth]
  );

  const logout = useCallback(() => {
    setToken(null);
    persistUser(null);
    setTokenState(null);
    setUser(null);
  }, []);

  const value = useMemo(
    () => ({
      user,
      token,
      loading,
      isAuthenticated: Boolean(token),
      isAdmin: user?.role === 'admin',
      login,
      register,
      logout,
      setUser,
    }),
    [user, token, loading, login, register, logout]
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth must be used within an AuthProvider');
  }
  return ctx;
}

export default AuthContext;
