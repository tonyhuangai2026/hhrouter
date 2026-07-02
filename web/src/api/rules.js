import client from './client';
import { normalizeList, unwrap } from './helpers';

// Routing rules resource API per Tech Design §8 (admin endpoints).
//   GET/POST/PUT/DELETE /api/rules
// Rule fields (§3): name, enabled, priority(asc = matched first),
// match { groups[], models[], min_tokens, max_tokens },
// target_channel_ids[], target_group.

export async function listRules(params = {}) {
  const { data } = await client.get('/rules', { params });
  return normalizeList(data);
}

export async function createRule(payload) {
  const { data } = await client.post('/rules', payload);
  return unwrap(data);
}

export async function updateRule(id, payload) {
  const { data } = await client.put(`/rules/${id}`, payload);
  return unwrap(data);
}

export async function deleteRule(id) {
  const { data } = await client.delete(`/rules/${id}`);
  return unwrap(data);
}

// Routing-classifier (probe) settings: { mock, url, region }.
export async function getRouterProbe() {
  const { data } = await client.get('/router-probe');
  return unwrap(data);
}

export async function setRouterProbe(payload) {
  const { data } = await client.put('/router-probe', payload);
  return unwrap(data);
}

// Test connectivity to a probe proxy URL (falls back to the saved URL when
// omitted). Returns { ok, latency_ms, error?, result? }.
export async function testRouterProbe(url) {
  const { data } = await client.post('/router-probe/test', { url: url || '' });
  return unwrap(data);
}

// Distinct routing groups in use (for the rule editor's group dropdown).
export async function listRuleGroups() {
  const { data } = await client.get('/rule-groups');
  return data?.groups || [];
}
