import client from './client';
import { normalizeList } from './helpers';

// Request logs API per Tech Design §8.
//   GET /api/logs — paginated request_logs with filters.
//
// Filter params (all optional):
//   page, page_size            pagination
//   start, end                 ISO timestamps for the selected time range
//   channel_id                 filter by upstream channel
//   model                      filter by model name
//   status                     success | error
//   user_id                    (admin only) filter by a specific user
//   is_test                    log type: omit/"all" = all, "true" = test-chat
//                              only, "false" = production only (Tech Design §3.3)
//
// request_log fields (§3): user_id, token_id (nullable: NULL for test-chat),
// channel_id, rule_id, model, upstream_model, inbound_format, prompt_tokens,
// completion_tokens, total_tokens, status, http_status, error_message,
// latency_ms, is_stream, is_test, created_at.

export async function listLogs(params = {}) {
  const { data } = await client.get('/logs', { params });
  return normalizeList(data);
}
