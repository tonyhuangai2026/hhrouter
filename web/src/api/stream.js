// SSE consumer for streaming chat completions.
//
// We cannot use the browser's EventSource because the backend endpoint requires
// a POST body and an Authorization: Bearer <jwt> header (EventSource only does
// GET and cannot set custom headers). Instead we use fetch() with
// response.body.getReader() and parse the `data:` lines incrementally.
//
// The expected wire format (OpenAI-style SSE) is a sequence of lines:
//   data: {"choices":[{"delta":{"content":"..."}}], "usage": {...}?}
//   data: [DONE]
// separated by blank lines. We accumulate text deltas and invoke callbacks.

import { getToken } from './client';

// Resolve a relative /api path against the current origin so fetch() (which
// does not share axios' baseURL) hits the same backend.
function resolveUrl(path) {
  if (/^https?:\/\//i.test(path)) return path;
  const base = path.startsWith('/') ? path : `/${path}`;
  return base;
}

// Extract a streamed text fragment from a parsed SSE chunk object.
// Handles OpenAI delta shape {choices:[{delta:{content}}]} and a few fallbacks.
function extractDeltaText(obj) {
  if (!obj || typeof obj !== 'object') return '';
  const choice = Array.isArray(obj.choices) ? obj.choices[0] : null;
  if (choice) {
    const delta = choice.delta || choice.message || null;
    if (delta) {
      const content = delta.content;
      if (typeof content === 'string') return content;
      // content may be an array of parts: [{type:'text',text:'...'}]
      if (Array.isArray(content)) {
        return content
          .map((p) => (p && typeof p.text === 'string' ? p.text : ''))
          .join('');
      }
    }
    if (typeof choice.text === 'string') return choice.text;
  }
  return '';
}

/**
 * Consume an SSE stream from a POST endpoint with JWT auth.
 *
 * @param {string} path        - relative (e.g. /api/...) or absolute URL.
 * @param {object} body        - JSON request body (will be stringified).
 * @param {object} handlers
 * @param {(text:string)=>void}  handlers.onDelta - called per incremental text chunk.
 * @param {(usage?:object)=>void} handlers.onDone - called once on [DONE]/stream end.
 * @param {(err:Error)=>void}     handlers.onError - called on any error/abort.
 * @param {AbortSignal} [signal] - optional AbortController signal to cancel.
 * @returns {Promise<void>}
 */
export async function streamChat(path, body, handlers = {}, signal) {
  const { onDelta, onDone, onError } = handlers;
  let lastUsage;
  let finished = false;

  const finish = () => {
    if (finished) return;
    finished = true;
    if (typeof onDone === 'function') onDone(lastUsage);
  };

  try {
    const token = getToken();
    const res = await fetch(resolveUrl(path), {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        Accept: 'text/event-stream',
      },
      body: JSON.stringify(body),
      signal,
    });

    if (!res.ok || !res.body) {
      // Try to surface the upstream/backend error text (often JSON or plain).
      let detail = '';
      try {
        detail = await res.text();
      } catch {
        /* ignore */
      }
      let message = detail;
      try {
        const parsed = JSON.parse(detail);
        message = parsed?.error?.message || parsed?.message || detail;
      } catch {
        /* not JSON — keep raw text */
      }
      const err = new Error(message || `HTTP ${res.status}`);
      err.status = res.status;
      throw err;
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder('utf-8');
    let buffer = '';

    // Process a single complete SSE event block (one or more `data:` lines).
    const handleEvent = (rawEvent) => {
      const lines = rawEvent.split('\n');
      for (const line of lines) {
        const trimmed = line.trimStart();
        if (!trimmed.startsWith('data:')) continue;
        const data = trimmed.slice(5).trim();
        if (data === '') continue;
        if (data === '[DONE]') {
          finish();
          return true; // signal done
        }
        try {
          const obj = JSON.parse(data);
          if (obj && obj.error) {
            const msg = obj.error.message || obj.error || 'stream error';
            throw new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
          }
          if (obj && obj.usage) lastUsage = obj.usage;
          const text = extractDeltaText(obj);
          if (text && typeof onDelta === 'function') onDelta(text);
        } catch (e) {
          // Re-throw genuine errors; ignore unparsable keep-alive/comment lines.
          if (e instanceof SyntaxError) continue;
          throw e;
        }
      }
      return false;
    };

    // eslint-disable-next-line no-constant-condition
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      // SSE events are delimited by a blank line (\n\n). Process complete ones.
      let sepIndex;
      // Normalize CRLF so the split is consistent.
      buffer = buffer.replace(/\r\n/g, '\n');
      while ((sepIndex = buffer.indexOf('\n\n')) !== -1) {
        const rawEvent = buffer.slice(0, sepIndex);
        buffer = buffer.slice(sepIndex + 2);
        if (handleEvent(rawEvent)) {
          return; // [DONE] reached
        }
      }
    }

    // Flush any trailing event that wasn't terminated by a blank line.
    if (buffer.trim()) {
      if (handleEvent(buffer)) return;
    }
    finish();
  } catch (err) {
    if (err && (err.name === 'AbortError' || signal?.aborted)) {
      // Cancellation is not a real error; treat the partial output as done.
      finish();
      return;
    }
    if (typeof onError === 'function') onError(err);
    else throw err;
  }
}

export default streamChat;
