import client from './client';
import { normalizeList, unwrap } from './helpers';

// Channels resource API per Tech Design §8 (admin endpoints).
//   GET/POST/PUT/DELETE /api/channels
//   POST /api/channels/:id/fetch-models
//   POST /api/channels/:id/test
// Channel fields (§3): name, type(openai|bedrock), base_url, key, region,
// models[], model_mapping, group, priority, weight, status.

export async function listChannels(params = {}) {
  const { data } = await client.get('/channels', { params });
  return normalizeList(data);
}

export async function createChannel(payload) {
  const { data } = await client.post('/channels', payload);
  return unwrap(data);
}

export async function updateChannel(id, payload) {
  const { data } = await client.put(`/channels/${id}`, payload);
  return unwrap(data);
}

export async function deleteChannel(id) {
  const { data } = await client.delete(`/channels/${id}`);
  return unwrap(data);
}

export async function fetchModels(id) {
  const { data } = await client.post(`/channels/${id}/fetch-models`);
  return unwrap(data);
}

export async function testChannel(id) {
  const { data } = await client.post(`/channels/${id}/test`);
  return unwrap(data);
}
