import React, {
  useState,
  useEffect,
  useRef,
  useCallback,
  useMemo,
  Suspense,
  lazy,
} from 'react';
import { useTranslation } from 'react-i18next';
import {
  Typography,
  Select,
  Switch,
  InputNumber,
  Input,
  Button,
  Space,
  Toast,
  Tag,
  Avatar,
  Spin,
  Upload,
  Tooltip,
} from '@douyinfe/semi-ui';
import {
  IconSend,
  IconDelete,
  IconStop,
  IconImage,
  IconUser,
  IconBolt,
  IconCopy,
  IconRefresh,
} from '@douyinfe/semi-icons';
import { listChannels } from '../api/channels';
import { testChat, testChatStream } from '../api/playground';
import { mapApiError } from '../api/helpers';

const { Title, Text } = Typography;

// react-markdown component overrides: open links safely in a new tab. We do NOT
// allow raw HTML (no rehype-raw), so model text cannot inject markup.
const MD_COMPONENTS = {
  a: ({ node, ...props }) => (
    <a {...props} target="_blank" rel="noopener noreferrer" />
  ),
};

// Lazily load the entire markdown/highlight stack (react-markdown + remark-gfm +
// rehype-highlight + lowlight/highlight.js) in a SINGLE dynamic chunk, fetched
// only when a conversation first renders. Keeping the plugin imports inside this
// async factory (rather than static top-level imports) is what actually keeps
// the heavy graph out of the main bundle. Mirrors Dashboard.jsx's lazy(echarts).
// rehype-highlight defaults give broad correct highlighting; we deliberately do
// NOT add rehype-raw so raw HTML in model output stays inert (XSS safety).
const MarkdownRenderer = lazy(async () => {
  const [{ default: ReactMarkdown }, { default: remarkGfm }, { default: rehypeHighlight }] =
    await Promise.all([
      import('react-markdown'),
      import('remark-gfm'),
      import('rehype-highlight'),
    ]);
  const remarkPlugins = [remarkGfm];
  const rehypePlugins = [[rehypeHighlight, { detect: true, ignoreMissing: true }]];
  return {
    default: function Markdown({ children }) {
      return (
        <ReactMarkdown
          remarkPlugins={remarkPlugins}
          rehypePlugins={rehypePlugins}
          components={MD_COMPONENTS}
        >
          {children}
        </ReactMarkdown>
      );
    },
  };
});

// Max image size for base64 inlining (data URL). Keep well under the backend's
// generous request-body cap; oversized images are rejected client-side.
const MAX_IMAGE_BYTES = 8 * 1024 * 1024; // 8MB
const MAX_IMAGE_LABEL = '8MB';

// Theme-aware syntax-highlight + markdown styling. We inject this once instead
// of importing a fixed highlight.js theme CSS file, so code blocks read well in
// both Semi light and dark (body[theme-mode="dark"]) modes and we stay within
// the files this task owns. Token names follow highlight.js' class set.
const MARKDOWN_STYLE_ID = 'pg-markdown-style';
const MARKDOWN_CSS = `
.pg-md { overflow-wrap: anywhere; word-break: break-word; }
.pg-md > :first-child { margin-top: 0; }
.pg-md > :last-child { margin-bottom: 0; }
.pg-md p { margin: 6px 0; line-height: 1.6; }
.pg-md ul, .pg-md ol { margin: 6px 0; padding-left: 22px; }
.pg-md li { margin: 2px 0; }
.pg-md h1, .pg-md h2, .pg-md h3, .pg-md h4 { margin: 12px 0 6px; line-height: 1.3; }
.pg-md a { color: var(--semi-color-link); text-decoration: none; }
.pg-md a:hover { text-decoration: underline; }
.pg-md blockquote {
  margin: 6px 0; padding: 2px 12px;
  border-left: 3px solid var(--semi-color-border);
  color: var(--semi-color-text-2);
}
.pg-md hr { border: none; border-top: 1px solid var(--semi-color-border); margin: 12px 0; }
.pg-md table { border-collapse: collapse; margin: 8px 0; width: auto; max-width: 100%; }
.pg-md th, .pg-md td {
  border: 1px solid var(--semi-color-border);
  padding: 6px 10px; text-align: left;
}
.pg-md th { background: var(--semi-color-fill-1); font-weight: 600; }
.pg-md :not(pre) > code {
  background: var(--semi-color-fill-1);
  border-radius: 4px; padding: 1px 5px;
  font-size: 0.92em;
  font-family: var(--semi-font-family-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
}
.pg-md pre {
  margin: 8px 0; padding: 12px;
  background: var(--semi-color-fill-0);
  border: 1px solid var(--semi-color-border);
  border-radius: 8px; overflow-x: auto;
}
.pg-md pre code {
  background: transparent; padding: 0; font-size: 0.9em;
  font-family: var(--semi-font-family-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
}
/* highlight.js tokens — light defaults (GitHub-like). */
.pg-md .hljs { color: var(--semi-color-text-0); background: transparent; }
.pg-md .hljs-comment, .pg-md .hljs-quote { color: #6a737d; font-style: italic; }
.pg-md .hljs-keyword, .pg-md .hljs-selector-tag, .pg-md .hljs-literal,
.pg-md .hljs-doctag, .pg-md .hljs-name { color: #d73a49; }
.pg-md .hljs-string, .pg-md .hljs-meta .hljs-string, .pg-md .hljs-regexp,
.pg-md .hljs-addition { color: #032f62; }
.pg-md .hljs-number, .pg-md .hljs-literal, .pg-md .hljs-symbol,
.pg-md .hljs-bullet { color: #005cc5; }
.pg-md .hljs-title, .pg-md .hljs-section, .pg-md .hljs-title.function_,
.pg-md .hljs-title.class_ { color: #6f42c1; }
.pg-md .hljs-attr, .pg-md .hljs-attribute, .pg-md .hljs-variable,
.pg-md .hljs-template-variable, .pg-md .hljs-property { color: #e36209; }
.pg-md .hljs-built_in, .pg-md .hljs-type, .pg-md .hljs-class .hljs-title { color: #005cc5; }
.pg-md .hljs-tag, .pg-md .hljs-deletion { color: #22863a; }
.pg-md .hljs-emphasis { font-style: italic; }
.pg-md .hljs-strong { font-weight: 700; }
/* Dark-mode token overrides (GitHub-dark-like) under Semi's dark body attr. */
body[theme-mode="dark"] .pg-md .hljs { color: var(--semi-color-text-0); }
body[theme-mode="dark"] .pg-md .hljs-comment,
body[theme-mode="dark"] .pg-md .hljs-quote { color: #8b949e; }
body[theme-mode="dark"] .pg-md .hljs-keyword,
body[theme-mode="dark"] .pg-md .hljs-selector-tag,
body[theme-mode="dark"] .pg-md .hljs-literal,
body[theme-mode="dark"] .pg-md .hljs-doctag,
body[theme-mode="dark"] .pg-md .hljs-name { color: #ff7b72; }
body[theme-mode="dark"] .pg-md .hljs-string,
body[theme-mode="dark"] .pg-md .hljs-meta .hljs-string,
body[theme-mode="dark"] .pg-md .hljs-regexp,
body[theme-mode="dark"] .pg-md .hljs-addition { color: #a5d6ff; }
body[theme-mode="dark"] .pg-md .hljs-number,
body[theme-mode="dark"] .pg-md .hljs-symbol,
body[theme-mode="dark"] .pg-md .hljs-bullet { color: #79c0ff; }
body[theme-mode="dark"] .pg-md .hljs-title,
body[theme-mode="dark"] .pg-md .hljs-section,
body[theme-mode="dark"] .pg-md .hljs-title.function_,
body[theme-mode="dark"] .pg-md .hljs-title.class_ { color: #d2a8ff; }
body[theme-mode="dark"] .pg-md .hljs-attr,
body[theme-mode="dark"] .pg-md .hljs-attribute,
body[theme-mode="dark"] .pg-md .hljs-variable,
body[theme-mode="dark"] .pg-md .hljs-template-variable,
body[theme-mode="dark"] .pg-md .hljs-property { color: #ffa657; }
body[theme-mode="dark"] .pg-md .hljs-built_in,
body[theme-mode="dark"] .pg-md .hljs-type { color: #79c0ff; }
body[theme-mode="dark"] .pg-md .hljs-tag,
body[theme-mode="dark"] .pg-md .hljs-deletion { color: #7ee787; }
/* Per-assistant hover actions (Copy / Regenerate): hidden until the row is
   hovered or a child button is focused, so they stay keyboard-accessible. */
.pg-actions { opacity: 0; transition: opacity 0.15s ease; }
.pg-row:hover .pg-actions,
.pg-actions:focus-within { opacity: 1; }
/* Streaming typing cursor. */
@keyframes pg-blink { 0%, 49% { opacity: 1; } 50%, 100% { opacity: 0; } }
.pg-cursor {
  display: inline-block; width: 7px; height: 1.05em;
  margin-left: 2px; vertical-align: text-bottom; border-radius: 1px;
  background: var(--semi-color-text-1);
  animation: pg-blink 1s step-end infinite;
}
/* ChatGPT-style shell: a fixed-height two-pane app filling the content area. */
.pg-shell {
  display: flex;
  height: 100%;
  min-height: 0;
  gap: 16px;
}
.pg-sidebar {
  width: 300px;
  flex: 0 0 300px;
  display: flex;
  flex-direction: column;
  min-height: 0;
  border: 1px solid var(--semi-color-border);
  border-radius: 12px;
  background: var(--semi-color-bg-1);
  overflow: hidden;
}
.pg-sidebar-head {
  padding: 14px 16px;
  border-bottom: 1px solid var(--semi-color-border);
}
.pg-sidebar-body {
  padding: 16px;
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}
.pg-field { margin-bottom: 16px; }
.pg-field-label { display: block; margin-bottom: 6px; }
.pg-main {
  flex: 1 1 auto;
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  border: 1px solid var(--semi-color-border);
  border-radius: 12px;
  background: var(--semi-color-bg-1);
  overflow: hidden;
}
.pg-main-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  border-bottom: 1px solid var(--semi-color-border);
  flex: 0 0 auto;
}
.pg-scroll {
  flex: 1 1 auto;
  overflow-y: auto;
  min-height: 0;
  padding: 20px 16px;
}
/* Center the conversation column like ChatGPT. */
.pg-thread { max-width: 760px; margin: 0 auto; }
.pg-empty {
  height: 100%;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  text-align: center;
  gap: 6px;
}
.pg-composer {
  flex: 0 0 auto;
  border-top: 1px solid var(--semi-color-border);
  padding: 12px 16px;
  background: var(--semi-color-bg-1);
}
.pg-composer-inner { max-width: 760px; margin: 0 auto; }
/* Responsive: collapse the sidebar above the chat on narrow viewports. */
@media (max-width: 720px) {
  .pg-shell { flex-direction: column; gap: 12px; height: auto; }
  .pg-sidebar { width: 100%; flex: none; }
  .pg-sidebar-body { max-height: 320px; }
  .pg-main { min-height: 70vh; }
}
`;

// Inject the markdown/highlight stylesheet once (idempotent across mounts).
function ensureMarkdownStyles() {
  if (typeof document === 'undefined') return;
  if (document.getElementById(MARKDOWN_STYLE_ID)) return;
  const el = document.createElement('style');
  el.id = MARKDOWN_STYLE_ID;
  el.textContent = MARKDOWN_CSS;
  document.head.appendChild(el);
}

// Read a File/Blob into a base64 data URL.
function fileToDataUrl(file) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result);
    reader.onerror = () => reject(reader.error || new Error('read failed'));
    reader.readAsDataURL(file);
  });
}

// Build the OpenAI-style `content` for a message. If there are images we emit a
// parts array; otherwise a plain string (both accepted by the backend).
function buildContent(text, images) {
  if (!images || images.length === 0) return text;
  const parts = [];
  if (text) parts.push({ type: 'text', text });
  for (const url of images) {
    parts.push({ type: 'image_url', image_url: { url } });
  }
  return parts;
}

// Extract the plain-text portion of a message content (string or parts[]).
function contentText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content
      .filter((p) => p && p.type === 'text' && typeof p.text === 'string')
      .map((p) => p.text)
      .join('');
  }
  return '';
}

// Does a message carry any sendable content (non-empty text or any image)?
function hasContent(content) {
  if (typeof content === 'string') return content.trim() !== '';
  if (Array.isArray(content)) {
    return content.some(
      (p) =>
        (p && p.type === 'image_url' && p.image_url?.url) ||
        (p && p.type === 'text' && typeof p.text === 'string' && p.text.trim() !== '')
    );
  }
  return false;
}

// EMPTY-REPLY FRONT-LINE GUARD: drop empty assistant turns from the history that
// gets re-sent upstream. An empty assistant message (no text, no image) would
// otherwise be serialized to a `{text:""}` / empty content block on the next
// turn and trip the Bedrock "ContentBlock must set one of..." error (the bug
// reported for turn 2). We never send those; the UI still shows a subtle hint.
function stripEmptyAssistants(history) {
  return history.filter((m) => m.role !== 'assistant' || hasContent(m.content));
}

// Prepend an optional system prompt to the outgoing history.
function withSystem(systemPrompt, history) {
  const sys = (systemPrompt || '').trim();
  if (!sys) return history;
  return [{ role: 'system', content: sys }, ...history];
}

// Render an image part (used inside user/assistant bubbles).
function ImagePart({ url }) {
  return (
    <img src={url} alt="" style={{ maxWidth: 240, maxHeight: 240, borderRadius: 8 }} />
  );
}

// Render a single assistant text body as Markdown (lazy). Falls back to plain
// pre-wrapped text while the markdown chunk loads.
function MarkdownBody({ text }) {
  return (
    <Suspense
      fallback={<span style={{ whiteSpace: 'pre-wrap' }}>{text}</span>}
    >
      <div className="pg-md">
        <MarkdownRenderer>{text}</MarkdownRenderer>
      </div>
    </Suspense>
  );
}

// Render message content. Assistant text is Markdown; user text stays plain
// (newlines preserved). Image parts render for both roles.
function MessageContent({ message }) {
  const { role, content } = message;
  const isAssistant = role === 'assistant';

  if (typeof content === 'string') {
    if (isAssistant) return <MarkdownBody text={content} />;
    return <span style={{ whiteSpace: 'pre-wrap' }}>{content}</span>;
  }

  if (Array.isArray(content)) {
    const text = contentText(content);
    const images = content.filter(
      (p) => p && p.type === 'image_url' && p.image_url?.url
    );
    return (
      <Space vertical align="start" spacing={6} style={{ width: '100%' }}>
        {text
          ? isAssistant
            ? <MarkdownBody text={text} />
            : <span style={{ whiteSpace: 'pre-wrap' }}>{text}</span>
          : null}
        {images.map((p, i) => (
          <ImagePart key={i} url={p.image_url.url} />
        ))}
      </Space>
    );
  }
  return null;
}

export default function Playground() {
  const { t } = useTranslation(['playground', 'common']);

  const [channels, setChannels] = useState([]);
  const [channelId, setChannelId] = useState(null);
  const [model, setModel] = useState('');
  const [manualModel, setManualModel] = useState(false);
  const [stream, setStream] = useState(true);
  const [maxTokens, setMaxTokens] = useState(undefined);
  const [temperature, setTemperature] = useState(undefined);
  const [systemPrompt, setSystemPrompt] = useState('');

  const [messages, setMessages] = useState([]); // {role, content, error?, empty?}
  const [draft, setDraft] = useState('');
  const [images, setImages] = useState([]); // array of data: / http URLs
  const [imageUrlDraft, setImageUrlDraft] = useState('');
  const [sending, setSending] = useState(false);
  const [usage, setUsage] = useState(null);

  const abortRef = useRef(null);
  const scrollRef = useRef(null);

  // Inject markdown styles once on mount.
  useEffect(() => {
    ensureMarkdownStyles();
  }, []);

  // Load channels on mount.
  useEffect(() => {
    let active = true;
    (async () => {
      try {
        const { items } = await listChannels({ page: 1, pageSize: 100 });
        if (active) setChannels(items || []);
      } catch (e) {
        Toast.error(mapApiError(e));
      }
    })();
    return () => {
      active = false;
    };
  }, []);

  const selectedChannel = useMemo(
    () => channels.find((c) => String(c.id) === String(channelId)) || null,
    [channels, channelId]
  );

  const channelModels = useMemo(
    () => (selectedChannel?.models && Array.isArray(selectedChannel.models) ? selectedChannel.models : []),
    [selectedChannel]
  );

  // When channel changes, reset the model selection sensibly.
  useEffect(() => {
    if (!selectedChannel) return;
    const list = channelModels;
    if (list.length > 0) {
      setManualModel(false);
      setModel((prev) => (list.includes(prev) ? prev : list[0]));
    } else {
      setManualModel(true);
      setModel('');
    }
  }, [channelId]); // eslint-disable-line react-hooks/exhaustive-deps

  // Auto-scroll to bottom on new content.
  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [messages, sending]);

  const channelOptions = useMemo(
    () =>
      channels.map((c) => ({
        label: `${c.name} (${c.type})`,
        value: String(c.id),
      })),
    [channels]
  );

  const modelOptions = useMemo(
    () => channelModels.map((m) => ({ label: m, value: m })),
    [channelModels]
  );

  const addImageFile = useCallback(
    async (file) => {
      if (!file) return;
      if (file.size > MAX_IMAGE_BYTES) {
        Toast.error(t('errors.imageTooLarge', { max: MAX_IMAGE_LABEL }));
        return;
      }
      try {
        const url = await fileToDataUrl(file);
        setImages((prev) => [...prev, url]);
      } catch {
        Toast.error(t('errors.imageReadFailed'));
      }
    },
    [t]
  );

  // Paste handler: capture images from clipboard.
  const handlePaste = useCallback(
    (e) => {
      const items = e.clipboardData?.items;
      if (!items) return;
      for (const item of items) {
        if (item.kind === 'file' && item.type.startsWith('image/')) {
          const file = item.getAsFile();
          if (file) {
            e.preventDefault();
            addImageFile(file);
          }
        }
      }
    },
    [addImageFile]
  );

  const addImageUrl = useCallback(() => {
    const url = imageUrlDraft.trim();
    if (!url) return;
    if (!/^https?:\/\//i.test(url) && !/^data:/i.test(url)) {
      Toast.error(t('input.imageUrlInvalid'));
      return;
    }
    setImages((prev) => [...prev, url]);
    setImageUrlDraft('');
  }, [imageUrlDraft, t]);

  const removeImage = useCallback((idx) => {
    setImages((prev) => prev.filter((_, i) => i !== idx));
  }, []);

  const resolvedModel = manualModel ? model.trim() : model;

  const handleStop = useCallback(() => {
    if (abortRef.current) {
      abortRef.current.abort();
      abortRef.current = null;
      Toast.info(t('toast.stopped'));
    }
  }, [t]);

  const handleClear = useCallback(() => {
    if (abortRef.current) {
      abortRef.current.abort();
      abortRef.current = null;
    }
    setMessages([]);
    setUsage(null);
    setSending(false);
    Toast.success(t('toast.cleared'));
  }, [t]);

  // Core completion runner shared by send + regenerate.
  //
  // `baseHistory` is the full conversation to display AND the basis for the
  // upstream request. Before sending we strip empty assistant turns (front-line
  // guard) and prepend the optional system prompt. A placeholder assistant
  // bubble is appended for streaming; when the reply ends empty we mark it so
  // the UI shows a "no output this turn" hint and it is excluded next turn.
  const runCompletion = useCallback(
    async (baseHistory) => {
      const outgoing = withSystem(systemPrompt, stripEmptyAssistants(baseHistory));
      const options = {
        model: resolvedModel,
        messages: outgoing,
        maxTokens,
        temperature,
      };

      setUsage(null);
      setSending(true);

      try {
        if (stream) {
          const assistantIndex = baseHistory.length;
          setMessages([...baseHistory, { role: 'assistant', content: '' }]);
          const controller = new AbortController();
          abortRef.current = controller;

          await testChatStream(
            channelId,
            options,
            {
              onDelta: (chunk) => {
                setMessages((prev) => {
                  const next = [...prev];
                  const cur = next[assistantIndex];
                  if (cur && cur.role === 'assistant') {
                    next[assistantIndex] = {
                      ...cur,
                      content: (cur.content || '') + chunk,
                    };
                  }
                  return next;
                });
              },
              onDone: (u) => {
                if (u) setUsage(u);
                // Mark an empty reply so the UI can hint and the next turn skips it.
                setMessages((prev) => {
                  const next = [...prev];
                  const cur = next[assistantIndex];
                  if (cur && cur.role === 'assistant' && !cur.error && !hasContent(cur.content)) {
                    next[assistantIndex] = { ...cur, content: '', empty: true };
                  }
                  return next;
                });
              },
              onError: (err) => {
                const msg = err?.message || mapApiError(err) || t('errors.sendFailed');
                Toast.error(msg);
                setMessages((prev) => {
                  const next = [...prev];
                  const cur = next[assistantIndex];
                  if (cur && cur.role === 'assistant' && !hasContent(cur.content)) {
                    next[assistantIndex] = { ...cur, content: msg, error: true };
                  }
                  return next;
                });
              },
            },
            controller.signal
          );
          abortRef.current = null;
        } else {
          const res = await testChat(channelId, options);
          const msg = res?.choices?.[0]?.message;
          const content = msg?.content ?? '';
          const empty = !hasContent(content);
          setMessages([
            ...baseHistory,
            { role: msg?.role || 'assistant', content: empty ? '' : content, empty },
          ]);
          if (res?.usage) setUsage(res.usage);
        }
      } catch (e) {
        const msg = mapApiError(e);
        Toast.error(msg || t('errors.sendFailed'));
        setMessages([...baseHistory, { role: 'assistant', content: msg, error: true }]);
      } finally {
        setSending(false);
        abortRef.current = null;
      }
    },
    [channelId, resolvedModel, systemPrompt, maxTokens, temperature, stream, t]
  );

  const handleSend = useCallback(() => {
    if (!channelId) {
      Toast.warning(t('errors.noChannel'));
      return;
    }
    if (!resolvedModel) {
      Toast.warning(t('errors.noModel'));
      return;
    }
    const text = draft.trim();
    if (!text && images.length === 0) {
      Toast.warning(t('errors.emptyMessage'));
      return;
    }

    const userMessage = { role: 'user', content: buildContent(text, images) };
    const baseHistory = [...messages, userMessage];
    setMessages(baseHistory);
    setDraft('');
    setImages([]);
    runCompletion(baseHistory);
  }, [channelId, resolvedModel, draft, images, messages, runCompletion, t]);

  // Regenerate the assistant message at `index`: drop it and everything after,
  // then resend using the conversation up to (but excluding) that turn.
  const handleRegenerate = useCallback(
    (index) => {
      if (sending) return;
      if (!channelId || !resolvedModel) {
        Toast.warning(t(!channelId ? 'errors.noChannel' : 'errors.noModel'));
        return;
      }
      const baseHistory = messages.slice(0, index);
      if (baseHistory.length === 0) return;
      setMessages(baseHistory);
      runCompletion(baseHistory);
    },
    [sending, channelId, resolvedModel, messages, runCompletion, t]
  );

  const handleCopy = useCallback(
    async (content) => {
      const text = contentText(content);
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(text);
        } else {
          const ta = document.createElement('textarea');
          ta.value = text;
          ta.style.position = 'fixed';
          ta.style.opacity = '0';
          document.body.appendChild(ta);
          ta.select();
          document.execCommand('copy');
          document.body.removeChild(ta);
        }
        Toast.success(t('toast.copied'));
      } catch {
        Toast.error(t('toast.copyFailed'));
      }
    },
    [t]
  );

  const handleKeyDown = useCallback(
    (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        if (!sending) handleSend();
      }
    },
    [handleSend, sending]
  );

  const lastIndex = messages.length - 1;
  const canClear = messages.length > 0 || sending;

  return (
    <div className="pg-shell">
      {/* ── Left: settings sidebar ─────────────────────────────── */}
      <aside className="pg-sidebar">
        <div className="pg-sidebar-head">
          <Title heading={4} style={{ margin: 0 }}>
            {t('config.title')}
          </Title>
          <Text type="tertiary" size="small">
            {t('subtitle')}
          </Text>
        </div>
        <div className="pg-sidebar-body">
          <div className="pg-field">
            <Text strong className="pg-field-label">
              {t('config.channel')}
            </Text>
            <Select
              style={{ width: '100%' }}
              placeholder={t('config.channelPlaceholder')}
              optionList={channelOptions}
              value={channelId}
              onChange={setChannelId}
              filter
            />
          </div>

          <div className="pg-field">
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                marginBottom: 6,
              }}
            >
              <Text strong>{t('config.model')}</Text>
              {channelModels.length > 0 ? (
                <Space spacing={6}>
                  <Text type="tertiary" size="small">
                    {t('config.modelManual')}
                  </Text>
                  <Switch
                    size="small"
                    checked={manualModel}
                    onChange={setManualModel}
                    aria-label="manual-model"
                  />
                </Space>
              ) : null}
            </div>
            {manualModel ? (
              <Input
                style={{ width: '100%' }}
                placeholder={t('config.modelManualPlaceholder')}
                value={model}
                onChange={setModel}
              />
            ) : (
              <Select
                style={{ width: '100%' }}
                placeholder={t('config.modelPlaceholder')}
                optionList={modelOptions}
                value={model}
                onChange={setModel}
                filter
              />
            )}
            {selectedChannel && channelModels.length === 0 ? (
              <Text
                type="warning"
                size="small"
                style={{ display: 'block', marginTop: 6 }}
              >
                {t('config.noModelsHint')}
              </Text>
            ) : null}
          </div>

          <div
            className="pg-field"
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
            }}
          >
            <Text strong>{t('config.stream')}</Text>
            <Switch checked={stream} onChange={setStream} />
          </div>

          <div className="pg-field">
            <Text strong className="pg-field-label">
              {t('config.maxTokens')}
            </Text>
            <InputNumber
              style={{ width: '100%' }}
              min={1}
              value={maxTokens}
              onChange={setMaxTokens}
              placeholder={t('common:labels.optional')}
            />
          </div>

          <div className="pg-field">
            <Text strong className="pg-field-label">
              {t('config.temperature')}
            </Text>
            <InputNumber
              style={{ width: '100%' }}
              min={0}
              max={2}
              step={0.1}
              value={temperature}
              onChange={setTemperature}
              placeholder={t('common:labels.optional')}
            />
          </div>

          <div className="pg-field" style={{ marginBottom: 0 }}>
            <Text strong className="pg-field-label">
              {t('config.system')}
            </Text>
            <Input
              mode="textarea"
              autosize={{ minRows: 2, maxRows: 6 }}
              value={systemPrompt}
              onChange={setSystemPrompt}
              placeholder={t('config.systemPlaceholder')}
            />
          </div>
        </div>
      </aside>

      {/* ── Right: chat pane ──────────────────────────────────── */}
      <section className="pg-main">
        <div className="pg-main-head">
          <Space spacing={8} align="center">
            <Avatar size="small" color="green">
              <IconBolt />
            </Avatar>
            <span style={{ fontWeight: 600 }}>{t('title')}</span>
            {usage ? (
              <Space spacing={4} wrap>
                <Tag size="small">
                  {t('usage.promptTokens')}: {usage.prompt_tokens ?? '-'}
                </Tag>
                <Tag size="small">
                  {t('usage.completionTokens')}: {usage.completion_tokens ?? '-'}
                </Tag>
                <Tag size="small" color="green">
                  {t('usage.totalTokens')}: {usage.total_tokens ?? '-'}
                </Tag>
              </Space>
            ) : null}
          </Space>
          <Button
            size="small"
            icon={<IconDelete />}
            theme="borderless"
            type="tertiary"
            onClick={handleClear}
            disabled={!canClear}
          >
            {t('actions.newChat')}
          </Button>
        </div>

        <div ref={scrollRef} className="pg-scroll">
          {messages.length === 0 ? (
            <div className="pg-empty">
              <Avatar size="large" color="green">
                <IconBolt />
              </Avatar>
              <Title heading={5} style={{ margin: '8px 0 0' }}>
                {t('status.emptyTitle')}
              </Title>
              <Text type="tertiary">{t('status.empty')}</Text>
            </div>
          ) : (
            <div className="pg-thread">
              <Space vertical align="start" spacing={16} style={{ width: '100%' }}>
                {messages.map((m, i) => {
                  const isUser = m.role === 'user';
                  const isAssistant = m.role === 'assistant';
                  const isStreamingTail =
                    isAssistant && sending && i === lastIndex && stream;
                  const showSpinner = isAssistant && isStreamingTail && !m.content;
                  return (
                    <div
                      key={i}
                      className="pg-row"
                      style={{
                        display: 'flex',
                        flexDirection: isUser ? 'row-reverse' : 'row',
                        gap: 10,
                        width: '100%',
                      }}
                    >
                      <Avatar size="small" color={isUser ? 'blue' : 'green'}>
                        {isUser ? <IconUser /> : <IconBolt />}
                      </Avatar>
                      <div style={{ maxWidth: '82%' }}>
                        <div
                          style={{
                            background: isUser
                              ? 'var(--semi-color-primary-light-default)'
                              : 'var(--semi-color-fill-0)',
                            color: m.error ? 'var(--semi-color-danger)' : undefined,
                            borderRadius: 12,
                            padding: '10px 14px',
                          }}
                        >
                          {showSpinner ? (
                            <Spin size="small" />
                          ) : m.empty ? (
                            <Text type="tertiary" style={{ fontStyle: 'italic' }}>
                              {t('status.noOutput')}
                            </Text>
                          ) : (
                            <span>
                              <MessageContent message={m} />
                              {isStreamingTail ? <span className="pg-cursor" /> : null}
                            </span>
                          )}
                        </div>
                        {isAssistant && !isStreamingTail ? (
                          <Space spacing={4} className="pg-actions" style={{ marginTop: 4 }}>
                            {!m.empty && !m.error ? (
                              <Tooltip content={t('actions.copy')}>
                                <Button
                                  size="small"
                                  theme="borderless"
                                  type="tertiary"
                                  icon={<IconCopy />}
                                  aria-label={t('actions.copy')}
                                  onClick={() => handleCopy(m.content)}
                                />
                              </Tooltip>
                            ) : null}
                            <Tooltip content={t('actions.regenerate')}>
                              <Button
                                size="small"
                                theme="borderless"
                                type="tertiary"
                                icon={<IconRefresh />}
                                aria-label={t('actions.regenerate')}
                                disabled={sending}
                                onClick={() => handleRegenerate(i)}
                              />
                            </Tooltip>
                          </Space>
                        ) : null}
                      </div>
                    </div>
                  );
                })}
              </Space>
            </div>
          )}
        </div>

        {/* Composer pinned to the bottom of the chat pane. */}
        <div className="pg-composer">
          <div className="pg-composer-inner">
            {images.length > 0 ? (
              <Space wrap style={{ marginBottom: 8 }}>
                {images.map((url, i) => (
                  <div key={i} style={{ position: 'relative' }}>
                    <img
                      src={url}
                      alt=""
                      style={{ width: 56, height: 56, objectFit: 'cover', borderRadius: 6 }}
                    />
                    <Button
                      size="small"
                      type="danger"
                      theme="solid"
                      icon={<IconDelete />}
                      onClick={() => removeImage(i)}
                      aria-label={t('input.removeImage')}
                      style={{ position: 'absolute', top: -6, right: -6, padding: 0, minWidth: 20, height: 20 }}
                    />
                  </div>
                ))}
              </Space>
            ) : null}

            <Input
              mode="textarea"
              autosize={{ minRows: 1, maxRows: 8 }}
              value={draft}
              onChange={setDraft}
              onKeyDown={handleKeyDown}
              onPaste={handlePaste}
              placeholder={t('input.placeholder')}
            />

            <Space
              style={{ width: '100%', justifyContent: 'space-between', marginTop: 8 }}
              align="center"
            >
              <Space spacing={4} align="center">
                <Upload
                  accept="image/*"
                  action=""
                  showUploadList={false}
                  customRequest={({ file }) => addImageFile(file.fileInstance)}
                >
                  <Tooltip content={t('input.uploadImage')}>
                    <Button
                      icon={<IconImage />}
                      theme="borderless"
                      type="tertiary"
                      aria-label={t('input.uploadImage')}
                    />
                  </Tooltip>
                </Upload>
                <Input
                  size="small"
                  value={imageUrlDraft}
                  onChange={setImageUrlDraft}
                  placeholder={t('input.imageUrlPlaceholder')}
                  style={{ width: 240 }}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      addImageUrl();
                    }
                  }}
                  suffix={
                    <Button size="small" theme="borderless" onClick={addImageUrl}>
                      {t('input.addImageUrl')}
                    </Button>
                  }
                />
              </Space>
              <Space>
                {sending && stream ? (
                  <Button icon={<IconStop />} type="danger" onClick={handleStop}>
                    {t('actions.stop')}
                  </Button>
                ) : null}
                <Button
                  icon={<IconSend />}
                  theme="solid"
                  type="primary"
                  loading={sending}
                  onClick={handleSend}
                >
                  {t('actions.send')}
                </Button>
              </Space>
            </Space>
          </div>
        </div>
      </section>
    </div>
  );
}
