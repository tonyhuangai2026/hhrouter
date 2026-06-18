import client from './client';
import { normalizeList, unwrap } from './helpers';

// Model-pricing resource API (USD billing). Admin endpoints:
//   GET    /api/pricing?channel_id=  → list price rows (a channel's, or all)
//   PUT    /api/pricing              → upsert one (channel_id, model, 4 prices)
//   DELETE /api/pricing/:id          → remove a price row
// All prices are micro-USD per 1,000,000 tokens (int64); the UI converts to/from
// USD so a single rounding source lives in the form layer.

export async function listPricing(channelId) {
  const params = channelId != null ? { channel_id: channelId } : {};
  const { data } = await client.get('/pricing', { params });
  return normalizeList(data);
}

export async function upsertPricing(payload) {
  // payload: { channel_id, model, input_micro_usd_per_m, output_micro_usd_per_m,
  //            cache_read_micro_usd_per_m, cache_write_micro_usd_per_m }
  const { data } = await client.put('/pricing', payload);
  return unwrap(data);
}

export async function deletePricing(id) {
  const { data } = await client.delete(`/pricing/${id}`);
  return unwrap(data);
}
