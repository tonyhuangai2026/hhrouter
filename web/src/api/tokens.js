import client from './client';
import { normalizeList, unwrap } from './helpers';

// Tokens (downstream API keys) resource API per Tech Design §8.
//   GET/POST/PUT/DELETE /api/tokens
// Token fields (§3): name, status(enabled|disabled|expired), quota(-1=unlimited),
// used_quota, expired_at, group, allowed_models[]. On create the full plaintext
// `sk-` key is returned exactly once.

export async function listTokens(params = {}) {
  const { data } = await client.get('/tokens', { params });
  return normalizeList(data);
}

export async function createToken(payload) {
  const { data } = await client.post('/tokens', payload);
  return unwrap(data);
}

export async function updateToken(id, payload) {
  const { data } = await client.put(`/tokens/${id}`, payload);
  return unwrap(data);
}

export async function deleteToken(id) {
  const { data } = await client.delete(`/tokens/${id}`);
  return unwrap(data);
}
