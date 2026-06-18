// Playground API — wraps POST /api/channels/:id/test-chat (admin-only).
//
// The endpoint accepts an OpenAI chat-style body:
//   { model?, messages:[{role, content: string | parts[]}], stream?, max_tokens?, temperature? }
// where parts support {type:"text",text} and {type:"image_url",image_url:{url}}.
//
// Non-streaming requests go through the shared axios client (JWT auto-injected).
// Streaming requests use fetch + getReader via stream.js (EventSource cannot
// POST or set the Authorization header).

import client from './client';
import { unwrap } from './helpers';
import { streamChat } from './stream';

// Build the request body shared by both transports.
function buildBody({ model, messages, stream, maxTokens, temperature }) {
  const body = { messages };
  if (model) body.model = model;
  if (stream) body.stream = true;
  if (maxTokens != null && maxTokens !== '') body.max_tokens = Number(maxTokens);
  if (temperature != null && temperature !== '') body.temperature = Number(temperature);
  return body;
}

/**
 * Non-streaming chat. Returns the OpenAI-style response object
 * { choices:[{message}], usage, ... }.
 */
export async function testChat(channelId, options) {
  const body = buildBody({ ...options, stream: false });
  const { data } = await client.post(`/channels/${channelId}/test-chat`, body);
  return unwrap(data);
}

/**
 * Streaming chat. Consumes SSE and invokes handlers.onDelta/onDone/onError.
 * Returns the promise from streamChat (resolves when the stream ends).
 *
 * @param {string|number} channelId
 * @param {object} options - { model, messages, maxTokens, temperature }
 * @param {object} handlers - { onDelta, onDone, onError }
 * @param {AbortSignal} [signal]
 */
export function testChatStream(channelId, options, handlers, signal) {
  const body = buildBody({ ...options, stream: true });
  return streamChat(`/api/channels/${channelId}/test-chat`, body, handlers, signal);
}
