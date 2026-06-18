import React, { useState, useEffect, useCallback, useMemo, Suspense, lazy } from 'react';
import { useParams, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  Typography,
  Space,
  Button,
  Tag,
  Tabs,
  TabPane,
  Banner,
  Spin,
  Empty,
  Toast,
} from '@douyinfe/semi-ui';
import { IconArrowLeft, IconCopy, IconLink } from '@douyinfe/semi-icons';
import { listTokens } from '../api/tokens';
import { mapApiError } from '../api/helpers';

const { Title, Text } = Typography;

// Default example model. Bare Bedrock Claude IDs get the inference-profile
// prefix auto-added by the backend adapter, so this works as written.
const DEFAULT_MODEL = 'anthropic.claude-opus-4-8';

// Tabs in display order; key matches the i18n tabs.* + clients.* keys.
const CLIENT_KEYS = ['claudeCode', 'codex', 'litellm', 'openai', 'curl'];

// Mask a token for display (mirrors Tokens.jsx maskKey).
function maskKey(record) {
  if (!record) return '';
  if (record.key_masked) return record.key_masked;
  if (record.masked_key) return record.masked_key;
  const raw = record.key || '';
  if (raw && raw.length > 10) return `${raw.slice(0, 6)}...${raw.slice(-4)}`;
  const prefix = record.key_prefix || 'sk-';
  const suffix = record.key_suffix || '';
  return suffix ? `${prefix}...${suffix}` : `${prefix}...`;
}

// Simple {{var}} interpolation over the i18n template strings. Values are
// substituted literally (no escaping needed — these render as code text).
function interpolate(tpl, vars) {
  return String(tpl).replace(/\{\{(\w+)\}\}/g, (m, k) => (k in vars ? vars[k] : m));
}

// Recursively flatten a React node tree to its raw text. rehype-highlight wraps
// code in nested <span> elements, so String(children) is NOT enough — we walk
// the tree and concatenate every string leaf. This yields the exact command
// text with no syntax-highlight markup (reviewer NOTE on the copy button).
function nodeToText(node) {
  if (node == null || node === false) return '';
  if (typeof node === 'string' || typeof node === 'number') return String(node);
  if (Array.isArray(node)) return node.map(nodeToText).join('');
  if (node.props && node.props.children != null) return nodeToText(node.props.children);
  return '';
}

// Theme-aware markdown + highlight styling, scoped to .tu-md. Injected once.
// Token colors mirror Playground's highlight.js theme so code reads well in
// both Semi light and dark modes.
const STYLE_ID = 'tu-markdown-style';
const MARKDOWN_CSS = `
.tu-md { overflow-wrap: anywhere; word-break: break-word; }
.tu-md > :first-child { margin-top: 0; }
.tu-md > :last-child { margin-bottom: 0; }
.tu-md p { margin: 8px 0; line-height: 1.6; }
.tu-md ul, .tu-md ol { margin: 8px 0; padding-left: 22px; }
.tu-md li { margin: 3px 0; }
.tu-md strong { font-weight: 600; }
.tu-md a { color: var(--semi-color-link); text-decoration: none; }
.tu-md a:hover { text-decoration: underline; }
.tu-md :not(pre) > code {
  background: var(--semi-color-fill-1);
  border-radius: 4px; padding: 1px 5px;
  font-size: 0.92em;
  font-family: var(--semi-font-family-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
}
/* Code block wrapper hosts the floating copy button. */
.tu-codewrap { position: relative; margin: 10px 0; }
.tu-codewrap pre {
  margin: 0; padding: 14px 16px;
  background: var(--semi-color-fill-0);
  border: 1px solid var(--semi-color-border);
  border-radius: 8px; overflow-x: auto;
}
.tu-codewrap pre code {
  background: transparent; padding: 0; font-size: 0.88em; line-height: 1.5;
  font-family: var(--semi-font-family-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
}
.tu-copy-btn {
  position: absolute; top: 8px; right: 8px;
  opacity: 0; transition: opacity 0.15s ease;
}
.tu-codewrap:hover .tu-copy-btn,
.tu-copy-btn:focus-within { opacity: 1; }
/* highlight.js tokens — light defaults (GitHub-like). */
.tu-md .hljs { color: var(--semi-color-text-0); background: transparent; }
.tu-md .hljs-comment, .tu-md .hljs-quote { color: #6a737d; font-style: italic; }
.tu-md .hljs-keyword, .tu-md .hljs-selector-tag, .tu-md .hljs-literal,
.tu-md .hljs-doctag, .tu-md .hljs-name { color: #d73a49; }
.tu-md .hljs-string, .tu-md .hljs-meta .hljs-string, .tu-md .hljs-regexp,
.tu-md .hljs-addition { color: #032f62; }
.tu-md .hljs-number, .tu-md .hljs-symbol, .tu-md .hljs-bullet { color: #005cc5; }
.tu-md .hljs-title, .tu-md .hljs-section, .tu-md .hljs-title.function_,
.tu-md .hljs-title.class_ { color: #6f42c1; }
.tu-md .hljs-attr, .tu-md .hljs-attribute, .tu-md .hljs-variable,
.tu-md .hljs-template-variable, .tu-md .hljs-property { color: #e36209; }
.tu-md .hljs-built_in, .tu-md .hljs-type { color: #005cc5; }
.tu-md .hljs-tag, .tu-md .hljs-deletion { color: #22863a; }
.tu-md .hljs-meta { color: #6a737d; }
/* Dark-mode token overrides (GitHub-dark-like). */
body[theme-mode="dark"] .tu-md .hljs { color: var(--semi-color-text-0); }
body[theme-mode="dark"] .tu-md .hljs-comment,
body[theme-mode="dark"] .tu-md .hljs-quote { color: #8b949e; }
body[theme-mode="dark"] .tu-md .hljs-keyword,
body[theme-mode="dark"] .tu-md .hljs-selector-tag,
body[theme-mode="dark"] .tu-md .hljs-literal,
body[theme-mode="dark"] .tu-md .hljs-doctag,
body[theme-mode="dark"] .tu-md .hljs-name { color: #ff7b72; }
body[theme-mode="dark"] .tu-md .hljs-string,
body[theme-mode="dark"] .tu-md .hljs-meta .hljs-string,
body[theme-mode="dark"] .tu-md .hljs-regexp,
body[theme-mode="dark"] .tu-md .hljs-addition { color: #a5d6ff; }
body[theme-mode="dark"] .tu-md .hljs-number,
body[theme-mode="dark"] .tu-md .hljs-symbol,
body[theme-mode="dark"] .tu-md .hljs-bullet { color: #79c0ff; }
body[theme-mode="dark"] .tu-md .hljs-title,
body[theme-mode="dark"] .tu-md .hljs-section,
body[theme-mode="dark"] .tu-md .hljs-title.function_,
body[theme-mode="dark"] .tu-md .hljs-title.class_ { color: #d2a8ff; }
body[theme-mode="dark"] .tu-md .hljs-attr,
body[theme-mode="dark"] .tu-md .hljs-attribute,
body[theme-mode="dark"] .tu-md .hljs-variable,
body[theme-mode="dark"] .tu-md .hljs-template-variable,
body[theme-mode="dark"] .tu-md .hljs-property { color: #ffa657; }
body[theme-mode="dark"] .tu-md .hljs-built_in,
body[theme-mode="dark"] .tu-md .hljs-type { color: #79c0ff; }
body[theme-mode="dark"] .tu-md .hljs-tag,
body[theme-mode="dark"] .tu-md .hljs-deletion { color: #7ee787; }
`;

function useMarkdownStyle() {
  useEffect(() => {
    if (document.getElementById(STYLE_ID)) return;
    const el = document.createElement('style');
    el.id = STYLE_ID;
    el.textContent = MARKDOWN_CSS;
    document.head.appendChild(el);
  }, []);
}

// Copy raw text to clipboard with a Toast (success / manual fallback). Mirrors
// the helper in Tokens.jsx.
function useCopy() {
  const { t } = useTranslation(['tokenUsage', 'common']);
  return useCallback(
    async (value) => {
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(value);
          Toast.success(t('copy.copied'));
          return;
        }
        throw new Error('clipboard unavailable');
      } catch {
        Toast.warning(t('copy.failed'));
      }
    },
    [t]
  );
}

// Lazy markdown stack (react-markdown + remark-gfm + rehype-highlight) in a
// single dynamic chunk, mirroring Playground.jsx. NO rehype-raw (XSS-safe).
// `pre` is overridden to add a per-block copy button that copies the RAW code
// text (extracted from the node tree, not the highlighted DOM).
const MarkdownRenderer = lazy(async () => {
  const [{ default: ReactMarkdown }, { default: remarkGfm }, { default: rehypeHighlight }] =
    await Promise.all([
      import('react-markdown'),
      import('remark-gfm'),
      import('rehype-highlight'),
    ]);
  const remarkPlugins = [remarkGfm];
  const rehypePlugins = [[rehypeHighlight, { detect: true, ignoreMissing: true }]];

  function CodeBlock({ copyLabel, onCopy, children }) {
    return (
      <div className="tu-codewrap">
        <Button
          className="tu-copy-btn"
          size="small"
          icon={<IconCopy />}
          onClick={() => onCopy(nodeToText(children))}
        >
          {copyLabel}
        </Button>
        <pre>{children}</pre>
      </div>
    );
  }

  return {
    default: function Markdown({ children, copyLabel, onCopy }) {
      const components = {
        a: ({ node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" />,
        pre: ({ node, children: preChildren }) => (
          <CodeBlock copyLabel={copyLabel} onCopy={onCopy}>
            {preChildren}
          </CodeBlock>
        ),
      };
      return (
        <ReactMarkdown
          remarkPlugins={remarkPlugins}
          rehypePlugins={rehypePlugins}
          components={components}
        >
          {children}
        </ReactMarkdown>
      );
    },
  };
});

export default function TokenUsage() {
  const { id } = useParams();
  const navigate = useNavigate();
  const { t } = useTranslation(['tokenUsage', 'common']);
  useMarkdownStyle();
  const copy = useCopy();

  const [token, setToken] = useState(null);
  const [loading, setLoading] = useState(true);
  const [notFound, setNotFound] = useState(false);

  // Base URL is the current origin so examples are correct on localhost and on
  // the deployed domain alike.
  const baseUrl = useMemo(() => window.location.origin, []);
  const apiBase = `${baseUrl}/v1`;

  useEffect(() => {
    let active = true;
    (async () => {
      setLoading(true);
      try {
        // The Tokens list is per-user and small; fetch a page and find by id.
        const { items } = await listTokens({ page: 1, page_size: 200 });
        if (!active) return;
        const found = (items || []).find((tk) => String(tk.id) === String(id));
        if (found) {
          setToken(found);
          setNotFound(false);
        } else {
          setNotFound(true);
        }
      } catch (e) {
        if (!active) return;
        Toast.error(mapApiError(e));
        setNotFound(true);
      } finally {
        if (active) setLoading(false);
      }
    })();
    return () => {
      active = false;
    };
  }, [id]);

  const group = token?.group || 'default';

  // Interpolation vars shared by every client template.
  const vars = useMemo(
    () => ({ baseUrl, apiBase, model: DEFAULT_MODEL, group }),
    [baseUrl, apiBase, group]
  );

  const goBack = useCallback(() => navigate('/tokens'), [navigate]);

  if (loading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
        <Spin size="large" />
      </div>
    );
  }

  if (notFound) {
    return (
      <div>
        <Button icon={<IconArrowLeft />} onClick={goBack} style={{ marginBottom: 16 }}>
          {t('notFound.back')}
        </Button>
        <Empty title={t('notFound.title')} description={t('notFound.desc')} style={{ padding: 60 }} />
      </div>
    );
  }

  return (
    <div style={{ maxWidth: 920, margin: '0 auto' }}>
      <Space style={{ marginBottom: 8 }}>
        <Button icon={<IconArrowLeft />} theme="borderless" onClick={goBack}>
          {t('back')}
        </Button>
      </Space>

      <Title heading={2} style={{ marginTop: 0 }}>
        {t('title')}
      </Title>
      <Text type="tertiary" style={{ display: 'block', marginBottom: 16 }}>
        {t('subtitle')}
      </Text>

      {/* Token context + base URL */}
      <Space wrap align="center" style={{ marginBottom: 12 }}>
        {token?.name ? (
          <Tag size="large" color="blue">
            {t('context.name')}: {token.name}
          </Tag>
        ) : null}
        <Tag size="large">
          {t('context.group')}: {group}
        </Tag>
        <Tag size="large">
          {t('context.key')}: <Text code>{maskKey(token)}</Text>
        </Tag>
        <Tag size="large">
          {t('baseUrlLabel')}: <Text code>{apiBase}</Text>
        </Tag>
        <Button size="small" icon={<IconLink />} onClick={() => copy(apiBase)}>
          {t('copyBaseUrl')}
        </Button>
      </Space>

      <Banner
        type="warning"
        description={t('keyNotice')}
        style={{ marginBottom: 8 }}
      />
      <Banner
        type="info"
        description={
          <span>
            {interpolate(t('modelNote'), vars)}
            <br />
            {/* routingNote uses **bold** markers; render the bolded group as
                a real <strong> rather than showing literal asterisks. */}
            {interpolate(t('routingNote'), vars)
              .split(/\*\*(.*?)\*\*/)
              .map((seg, i) => (i % 2 === 1 ? <strong key={i}>{seg}</strong> : seg))}
          </span>
        }
        style={{ marginBottom: 16 }}
      />

      <Tabs type="line">
        {CLIENT_KEYS.map((ck) => (
          <TabPane tab={t(`tabs.${ck}`)} itemKey={ck} key={ck}>
            <div className="tu-md" style={{ paddingTop: 8 }}>
              <Suspense fallback={<Spin />}>
                <MarkdownRenderer copyLabel={t('copy.button')} onCopy={copy}>
                  {interpolate(t(`clients.${ck}.body`), vars)}
                </MarkdownRenderer>
              </Suspense>
            </div>
          </TabPane>
        ))}
      </Tabs>
    </div>
  );
}
