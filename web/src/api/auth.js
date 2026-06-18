import client from './client';

// Auth + setup endpoints per Tech Design §8 (management API).

// GET /api/setup/status -> whether any users already exist.
// Expected shape (best-effort): { hasUsers: boolean, registerEnabled?: boolean, systemName?: string }
export async function getSetupStatus() {
  const { data } = await client.get('/setup/status');
  return data;
}

// POST /api/auth/login -> { token, user }
export async function login(payload) {
  const { data } = await client.post('/auth/login', payload);
  return data;
}

// POST /api/auth/register -> { token, user } (first registered user becomes admin)
export async function register(payload) {
  const { data } = await client.post('/auth/register', payload);
  return data;
}

// GET /api/user/self -> current user
export async function getSelf() {
  const { data } = await client.get('/user/self');
  return data;
}
