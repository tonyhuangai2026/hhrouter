import axios from 'axios';

// JWT storage key shared with AuthContext.
export const TOKEN_STORAGE_KEY = 'arp_jwt';

export function getToken() {
  return localStorage.getItem(TOKEN_STORAGE_KEY);
}

export function setToken(token) {
  if (token) {
    localStorage.setItem(TOKEN_STORAGE_KEY, token);
  } else {
    localStorage.removeItem(TOKEN_STORAGE_KEY);
  }
}

// Axios instance — baseURL=/api per Tech Design §9.
const client = axios.create({
  baseURL: '/api',
  timeout: 30000,
});

// Request interceptor: inject Authorization: Bearer <jwt> from localStorage.
client.interceptors.request.use((config) => {
  const token = getToken();
  if (token) {
    config.headers = config.headers || {};
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

// Response interceptor: on 401, clear token and redirect to /login.
client.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response && error.response.status === 401) {
      setToken(null);
      // Avoid redirect loops if we are already on an auth page.
      const path = window.location.pathname;
      if (path !== '/login' && path !== '/register' && path !== '/setup') {
        window.location.assign('/login');
      }
    }
    return Promise.reject(error);
  }
);

export default client;
