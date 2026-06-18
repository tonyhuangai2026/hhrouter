import client from './client';
import { normalizeList, unwrap } from './helpers';

// Users admin resource API (Tech Design §3). All endpoints are admin-only
// (JWTAuth + AdminOnly) unless noted.
//   GET    /api/users                    list (server-side pagination/search/filter/sort)
//   POST   /api/users                    create a user
//   PUT    /api/users/:id                update role/status/quota/group/display_name/email
//   DELETE /api/users/:id                delete (cascades the user's tokens)
//   POST   /api/users/:id/reset-password reset password (-> plaintext, returned once)
//   POST   /api/users/:id/quota          quota op (add | set | reset_used)
//
// UserView fields (no password): id, username, display_name, email, role
// (admin|user), status (enabled|disabled), quota (-1 = unlimited), used_quota,
// group, last_login_at, created_at.

// List users. Passes through page/page_size/search/role/status/sort/order to
// the backend; returns { items, total, page, page_size }. `normalizeList`
// already understands the { items, total } envelope; page/page_size are echoed
// back so the caller can drive server-side pagination.
export async function listUsers(params = {}) {
  const { data } = await client.get('/users', { params });
  const { items, total } = normalizeList(data);
  const root = data && data.data && !Array.isArray(data.data) ? data.data : data || {};
  return {
    items,
    total,
    page: Number(root.page) || Number(params.page) || 1,
    page_size: Number(root.page_size) || Number(params.page_size) || items.length,
  };
}

// Create a user. Body: { username, password?, display_name, email, role,
// status, quota, group }. When password is empty the backend generates one.
export async function createUser(payload) {
  const { data } = await client.post('/users', payload);
  return unwrap(data);
}

// Update a user. Accepts any subset of { role, status, quota, group,
// display_name, email }.
export async function updateUser(id, payload) {
  const { data } = await client.put(`/users/${id}`, payload);
  return unwrap(data);
}

// Delete a user. The backend cascades the user's tokens and enforces
// self-protection + last-admin guards (409 on violation).
export async function deleteUser(id) {
  const { data } = await client.delete(`/users/${id}`);
  return unwrap(data);
}

// Reset a user's password. Body: { password? } — empty -> backend generates a
// strong temporary password. Returns { password: "<plaintext once>" }.
export async function resetUserPassword(id, { password } = {}) {
  const { data } = await client.post(`/users/${id}/reset-password`, { password });
  return unwrap(data);
}

// Quota operation. Body: { op: 'add' | 'set' | 'reset_used', amount? }.
// 'reset_used' needs no amount. Returns the updated UserView.
export async function userQuotaOp(id, { op, amount } = {}) {
  const body = { op };
  if (amount != null) body.amount = amount;
  const { data } = await client.post(`/users/${id}/quota`, body);
  return unwrap(data);
}
