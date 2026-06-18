import client from './client';
import { unwrap } from './helpers';

// Dashboard analytics API per Tech Design §8 (admin endpoints, JWT).
//   GET /api/dashboard/summary    — totals + success rate + token split + avg latency
//   GET /api/dashboard/timeseries — request count & token usage bucketed over time
//
// Filter params (all optional; backend scopes to the current user unless the
// caller is admin and passes user_id):
//   start, end      ISO timestamps for the selected time range
//   channel_id      filter by upstream channel
//   model           filter by model name
//   status          success | error
//   user_id         (admin only) filter by a specific user
//   group_by        timeseries grouping dimension: channel | model (timeseries)
//   interval        bucket size: hour | day (timeseries)

export async function getSummary(params = {}) {
  const { data } = await client.get('/dashboard/summary', { params });
  return unwrap(data);
}

export async function getTimeseries(params = {}) {
  const { data } = await client.get('/dashboard/timeseries', { params });
  return unwrap(data);
}
