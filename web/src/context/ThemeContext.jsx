import React, {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  useMemo,
} from 'react';

const ThemeContext = createContext(null);

const THEME_STORAGE_KEY = 'arp_theme';

// Semi Design switches dark mode via body[theme-mode="dark"].
function applyTheme(mode) {
  const body = document.body;
  if (mode === 'dark') {
    body.setAttribute('theme-mode', 'dark');
  } else {
    body.removeAttribute('theme-mode');
  }
}

export function ThemeProvider({ children }) {
  const [mode, setMode] = useState(
    () => localStorage.getItem(THEME_STORAGE_KEY) || 'light'
  );

  useEffect(() => {
    applyTheme(mode);
    localStorage.setItem(THEME_STORAGE_KEY, mode);
  }, [mode]);

  const toggleTheme = useCallback(() => {
    setMode((prev) => (prev === 'dark' ? 'light' : 'dark'));
  }, []);

  const value = useMemo(
    () => ({ mode, isDark: mode === 'dark', toggleTheme, setMode }),
    [mode, toggleTheme]
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme() {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error('useTheme must be used within a ThemeProvider');
  }
  return ctx;
}

export default ThemeContext;
