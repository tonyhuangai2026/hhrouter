import React, { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import {
  Typography,
  Space,
  Button,
  Card,
  RadioGroup,
  Radio,
  DatePicker,
  Select,
  Table,
  Tag,
  Tooltip,
  Toast,
} from '@douyinfe/semi-ui';
import { IconRefresh } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../context/AuthContext';
import { mapApiError } from '../api/helpers';
import { listLogs } from '../api/logs';
import { listChannels } from '../api/channels';
import { listUsers } from '../api/users';
import { formatUSD } from '../utils/money';

const { Title, Text } = Typography;

// ---------------------------------------------------------------------------
// Scoped table styling (mirrors the inject-once <style> pattern used by
// Playground/TokenUsage). Striped rows, row hover, a bold sticky-ish header,
// tighter cells, and monospaced numerics for the tokens/latency columns so the
// request log reads like a proper telemetry table rather than plain text.
// ---------------------------------------------------------------------------
const LOG_STYLE_ID = 'lg-table-style';
const LOG_CSS = `
.lg-card .semi-table-thead > tr > th {
  background: var(--semi-color-fill-0);
  font-weight: 600;
  color: var(--semi-color-text-1);
  text-transform: uppercase;
  font-size: 11px;
  letter-spacing: 0.04em;
  white-space: nowrap;
}
.lg-card .semi-table-tbody > tr > td { padding-top: 10px; padding-bottom: 10px; }
/* Zebra striping on even rows (Semi rows are .semi-table-row). */
.lg-card .semi-table-tbody > .semi-table-row:nth-child(even) > td {
  background: var(--semi-color-fill-0);
}
.lg-card .semi-table-tbody > .semi-table-row:hover > td {
  background: var(--semi-color-primary-light-default) !important;
}
.lg-mono {
  font-family: var(--semi-font-family-mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
}
/* Latency / status leading dot. */
.lg-dot {
  display: inline-block; width: 8px; height: 8px; border-radius: 50%;
  margin-right: 6px; vertical-align: middle;
}
.lg-time-date { font-weight: 600; line-height: 1.25; }
.lg-time-clock { color: var(--semi-color-text-2); font-size: 12px; line-height: 1.2; }
/* Token segment separators. */
.lg-tok-sep { color: var(--semi-color-text-3); margin: 0 4px; }
/* Fill-height layout: the page is a flex column that fills the (already
   flex-grown) AppLayout Content; the table card grows to take the slack so the
   rows scroll internally and the pager pins to the card bottom. */
.lg-page { display: flex; flex-direction: column; height: 100%; min-height: 0; }
.lg-table-card { flex: 1 1 auto; min-height: 0; overflow: hidden; display: flex; flex-direction: column; }
.lg-table-card > .semi-card { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; }
.lg-table-card .semi-card-body { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; }
/* TTFT caption under the latency value. */
.lg-ttft-cap { color: var(--semi-color-text-2); font-size: 11px; line-height: 1.1; }
`;

function useLogTableStyle() {
  useEffect(() => {
    if (document.getElementById(LOG_STYLE_ID)) return;
    const el = document.createElement('style');
    el.id = LOG_STYLE_ID;
    el.textContent = LOG_CSS;
    document.head.appendChild(el);
  }, []);
}

// Humanize a token count: 1234 -> "1.2k", 26 -> "26". Keeps small values exact.
function humanTokens(n) {
  const v = Number(n || 0);
  if (v < 1000) return String(v);
  if (v < 10000) return (v / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
  return Math.round(v / 1000) + 'k';
}

// Upstream channel-type → tag color, so openai/bedrock/anthropic are visually
// distinct in the listing.
const UPSTREAM_COLORS = {
  openai: 'green',
  bedrock: 'orange',
  anthropic: 'violet',
};

// Total-latency severity → semantic color. Fast (<800ms) green, medium (<2500ms)
// amber, slow red. Used for the leading dot + value tint on non-stream rows.
function latencyColor(ms) {
  if (ms == null) return 'var(--semi-color-text-3)';
  if (ms < 800) return 'var(--semi-color-success)';
  if (ms < 2500) return 'var(--semi-color-warning)';
  return 'var(--semi-color-danger)';
}

// TTFT (time-to-first-token) severity → color. LLM first-token latency is
// naturally on the order of seconds, so it uses far more lenient thresholds than
// total latency: <3s green, 3-6s amber, >6s red.
function ttftColor(ms) {
  if (ms == null) return 'var(--semi-color-text-3)';
  if (ms < 3000) return 'var(--semi-color-success)';
  if (ms < 6000) return 'var(--semi-color-warning)';
  return 'var(--semi-color-danger)';
}

// ---------------------------------------------------------------------------
// Time range presets. "custom" lets the user pick an explicit [start, end].
// Mirrors the Dashboard so both pages share the same range semantics.
// ---------------------------------------------------------------------------
const RANGE_VALUES = ['today', '7d', '30d', 'custom'];
const DAY_MS = 24 * 60 * 60 * 1000;
const PAGE_SIZE = 20;

function presetRange(preset) {
  const end = new Date();
  const start = new Date();
  if (preset === 'today') {
    start.setHours(0, 0, 0, 0);
  } else if (preset === '7d') {
    start.setTime(end.getTime() - 7 * DAY_MS);
  } else if (preset === '30d') {
    start.setTime(end.getTime() - 30 * DAY_MS);
  }
  return [start, end];
}

function fmtNum(n) {
  return Number(n || 0).toLocaleString();
}

// ---------------------------------------------------------------------------
// Standalone request-log page (split out of the Dashboard). Server-side
// pagination + the same filter set the dashboard exposed: time range, channel,
// model, status, user (admin), and a prod/test type filter.
// Non-admins are scoped to their own logs by the backend regardless of filters.
// ---------------------------------------------------------------------------
export default function Logs() {
  const { t } = useTranslation(['logs', 'common']);
  const { isAdmin } = useAuth();
  useLogTableStyle();

  // Measure the table card's body so the Table can scroll internally (rows
  // scroll, header + pager stay pinned). The scroll body needs an explicit pixel
  // height; we observe the card body and subtract the header (~44px) and pager
  // (~64px) chrome so only the row area scrolls. Defaults to a sane min before
  // the first measurement.
  const tableCardRef = useRef(null);
  const [scrollY, setScrollY] = useState(360);
  useEffect(() => {
    const el = tableCardRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      const h = entries[0]?.contentRect?.height ?? 0;
      // Reserve room for the sticky header + pager; clamp to a usable minimum.
      const usable = Math.max(160, Math.floor(h - 44 - 64));
      setScrollY(usable);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Filters / controls.
  const [preset, setPreset] = useState('7d');
  const [customRange, setCustomRange] = useState(() => presetRange('7d'));
  const [channelId, setChannelId] = useState(undefined);
  const [model, setModel] = useState(undefined);
  const [status, setStatus] = useState('');
  const [userId, setUserId] = useState(undefined); // admin only
  // Type filter: 'all' (default), 'prod' (is_test=false) or 'test' (is_test=true).
  const [logType, setLogType] = useState('all');

  // Data.
  const [logs, setLogs] = useState([]);
  const [logTotal, setLogTotal] = useState(0);
  const [logPage, setLogPage] = useState(1);
  const [logLoading, setLogLoading] = useState(false);

  // Filter option sources (admin can list channels/users; normal users cannot).
  const [channelOptions, setChannelOptions] = useState([]);
  const [userOptions, setUserOptions] = useState([]);

  // Translated option lists (rebuild on language change via the t dep).
  const rangeOptions = useMemo(
    () => RANGE_VALUES.map((value) => ({ value, label: t(`range.${value}`) })),
    [t]
  );
  const statusOptions = useMemo(
    () => [
      { value: '', label: t('status.all') },
      { value: 'success', label: t('status.success') },
      { value: 'error', label: t('status.error') },
    ],
    [t]
  );
  const logTypeOptions = useMemo(
    () => [
      { value: 'all', label: t('type.all') },
      { value: 'prod', label: t('type.production') },
      { value: 'test', label: t('type.test') },
    ],
    [t]
  );

  // Resolve the active [start, end] for the current preset.
  const range = useMemo(() => {
    if (preset === 'custom') return customRange;
    return presetRange(preset);
  }, [preset, customRange]);

  const rangeParams = useMemo(() => {
    const [start, end] = range;
    if (!start || !end) return {};
    return {
      start: new Date(start).toISOString(),
      end: new Date(end).toISOString(),
    };
  }, [range]);

  // Common filter params (without pagination / type — those are merged in load).
  const filterParams = useMemo(() => {
    const p = { ...rangeParams };
    if (channelId != null) p.channel_id = channelId;
    if (model) p.model = model;
    if (status) p.status = status;
    if (isAdmin && userId != null) p.user_id = userId;
    return p;
  }, [rangeParams, channelId, model, status, isAdmin, userId]);

  // Load channel & user filter options once (admin only — these endpoints are
  // admin-scoped). Failures are non-fatal: the filters simply stay empty.
  useEffect(() => {
    if (!isAdmin) return;
    let active = true;
    (async () => {
      try {
        const { items } = await listChannels({ page: 1, page_size: 200 });
        if (active) {
          setChannelOptions(
            (items || []).map((c) => ({ label: c.name || `#${c.id}`, value: c.id }))
          );
        }
      } catch {
        /* non-admin or endpoint unavailable — ignore */
      }
      try {
        const { items } = await listUsers({ page: 1, page_size: 200 });
        if (active) {
          setUserOptions(
            (items || []).map((u) => ({
              label: u.username || u.display_name || `#${u.id}`,
              value: u.id,
            }))
          );
        }
      } catch {
        /* ignore */
      }
    })();
    return () => {
      active = false;
    };
  }, [isAdmin]);

  const loadLogs = useCallback(
    async (targetPage = 1) => {
      setLogLoading(true);
      try {
        // Map the type filter onto the backend is_test param: 'all' sends
        // nothing (server default = all rows), prod/test pin is_test=false/true.
        const logParams = { ...filterParams, page: targetPage, page_size: PAGE_SIZE };
        if (logType === 'prod') logParams.is_test = 'false';
        else if (logType === 'test') logParams.is_test = 'true';
        const { items, total } = await listLogs(logParams);
        setLogs(items);
        setLogTotal(total);
        setLogPage(targetPage);
      } catch (e) {
        Toast.error(mapApiError(e) || t('errors.loadLogs'));
        setLogs([]);
        setLogTotal(0);
      } finally {
        setLogLoading(false);
      }
    },
    [filterParams, logType, t]
  );

  // Reload (back to page 1) whenever any filter changes.
  const filtersKey = JSON.stringify(filterParams) + '|' + logType;
  useEffect(() => {
    loadLogs(1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filtersKey]);

  const columns = useMemo(() => {
    const cols = [
      {
        title: t('columns.time'),
        dataIndex: 'created_at',
        width: 130,
        render: (v, r) => {
          const ts = v ?? r.createdAt;
          if (ts == null) return '-';
          const d = new Date(ts);
          if (Number.isNaN(d.getTime())) return String(ts);
          const date = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
          const clock = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
          return (
            <div>
              <div className="lg-time-date">{date}</div>
              <div className="lg-time-clock">{clock}</div>
            </div>
          );
        },
      },
    ];
    if (isAdmin) {
      cols.push({
        title: t('columns.user'),
        width: 120,
        render: (_, r) => {
          const u = r.username || r.user?.username || r.user_id || r.userId;
          return u ? <Text>{u}</Text> : <Text type="tertiary">-</Text>;
        },
      });
    }
    cols.push(
      {
        title: t('columns.model'),
        width: 220,
        render: (_, r) => {
          const m = r.model;
          if (!m) return <Text type="tertiary">-</Text>;
          return (
            <Tag color="violet" size="small" className="lg-mono" style={{ maxWidth: 200 }}>
              {m}
            </Tag>
          );
        },
      },
      {
        title: t('columns.channel'),
        width: 140,
        render: (_, r) => {
          const ch = r.channel_name || r.channel?.name || r.channel_id || r.channelId;
          return ch ? <Tag color="blue" size="small">{ch}</Tag> : <Text type="tertiary">-</Text>;
        },
      },
      {
        title: t('columns.upstream'),
        dataIndex: 'channel_type',
        width: 110,
        render: (v, r) => {
          // The upstream kind that actually served the request (openai / bedrock
          // / anthropic), more meaningful than the inbound dialect (which for
          // test-chat is always openai).
          const ty = v ?? r.channelType;
          if (!ty) return <Text type="tertiary">-</Text>;
          return (
            <Tag color={UPSTREAM_COLORS[ty] || 'grey'} size="small" type="light">
              {ty}
            </Tag>
          );
        },
      },
      {
        title: t('columns.tokens'),
        width: 170,
        render: (_, r) => {
          const p = r.prompt_tokens ?? r.promptTokens ?? 0;
          const c = r.completion_tokens ?? r.completionTokens ?? 0;
          const tot = r.total_tokens ?? r.totalTokens ?? p + c;
          if (!p && !c && !tot) return <Text type="tertiary">-</Text>;
          return (
            <Tooltip content={`${t('columns.tokens')}: ${fmtNum(p)} / ${fmtNum(c)} / ${fmtNum(tot)}`}>
              <span className="lg-mono" style={{ fontSize: 13 }}>
                <span style={{ color: 'var(--semi-color-primary)' }}>{humanTokens(p)}</span>
                <span className="lg-tok-sep">/</span>
                <span style={{ color: 'var(--semi-color-success)' }}>{humanTokens(c)}</span>
                <span className="lg-tok-sep">/</span>
                <span style={{ fontWeight: 600 }}>{humanTokens(tot)}</span>
              </span>
            </Tooltip>
          );
        },
      },
      {
        title: t('columns.latency'),
        width: 120,
        render: (_, r) => {
          const stream = r.is_stream ?? r.isStream;
          const ttft = r.first_token_ms ?? r.firstTokenMs;
          const total = r.latency_ms ?? r.latencyMs;
          // Stream rows: total latency runs until the connection closes and is
          // meaningless, so show time-to-first-token instead (total in tooltip).
          if (stream && ttft != null) {
            const color = ttftColor(ttft);
            return (
              <Tooltip content={t('columns.latencyTooltipStream', { ttft: fmtNum(ttft), total: fmtNum(total ?? 0) })}>
                <div style={{ whiteSpace: 'nowrap' }}>
                  <span className="lg-mono" style={{ color, fontSize: 13 }}>
                    <span className="lg-dot" style={{ background: color }} />
                    {fmtNum(ttft)} ms
                  </span>
                  <div className="lg-ttft-cap">{t('columns.ttftLabel')}</div>
                </div>
              </Tooltip>
            );
          }
          // Non-stream (or legacy rows without a TTFT): total latency is the
          // meaningful figure.
          if (total != null) {
            const color = latencyColor(total);
            return (
              <Tooltip content={t('columns.latencyTooltipTotal', { total: fmtNum(total) })}>
                <span className="lg-mono" style={{ color, fontSize: 13, whiteSpace: 'nowrap' }}>
                  <span className="lg-dot" style={{ background: color }} />
                  {fmtNum(total)} ms
                </span>
              </Tooltip>
            );
          }
          return <Text type="tertiary">-</Text>;
        },
      },
      {
        title: t('columns.cost'),
        width: 110,
        render: (_, r) => {
          const cost = r.cost_micro_usd ?? r.costMicroUSD;
          if (cost == null) return <Text type="tertiary">-</Text>;
          const p = r.prompt_tokens ?? r.promptTokens ?? 0;
          const c = r.completion_tokens ?? r.completionTokens ?? 0;
          const cr = r.cache_read_tokens ?? r.cacheReadTokens ?? 0;
          const cw = r.cache_write_tokens ?? r.cacheWriteTokens ?? 0;
          return (
            <Tooltip content={t('columns.costTooltip', { prompt: fmtNum(p), completion: fmtNum(c), cacheRead: fmtNum(cr), cacheWrite: fmtNum(cw) })}>
              <span className="lg-mono" style={{ fontSize: 13, fontWeight: 600, whiteSpace: 'nowrap' }}>
                {formatUSD(cost)}
              </span>
            </Tooltip>
          );
        },
      },
      {
        title: t('columns.status'),
        dataIndex: 'status',
        width: 130,
        render: (s, r) => {
          const ok = s === 'success';
          const http = r.http_status ?? r.httpStatus;
          const label = s || t('statusUnknown');
          return (
            <Tag color={ok ? 'green' : 'red'} type="light" prefixIcon={<span className="lg-dot" style={{ background: ok ? 'var(--semi-color-success)' : 'var(--semi-color-danger)', marginRight: 0 }} />}>
              <span style={{ fontWeight: 600 }}>{label}</span>
              {http ? <span className="lg-mono" style={{ marginLeft: 6, opacity: 0.85 }}>{http}</span> : null}
            </Tag>
          );
        },
      }
    );
    return cols;
  }, [isAdmin, t]);

  const resetFilters = useCallback(() => {
    setChannelId(undefined);
    setModel(undefined);
    setStatus('');
    setUserId(undefined);
    setLogType('all');
  }, []);

  return (
    <div className="lg-page">
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 4 }}>
        <Title heading={2}>{t('title')}</Title>
        <Button icon={<IconRefresh />} onClick={() => loadLogs(logPage)}>
          {t('actions.refresh')}
        </Button>
      </Space>
      <Text type="tertiary" style={{ display: 'block', marginBottom: 16 }}>
        {t('subtitle')}
      </Text>

      {/* Time range + filters */}
      <Card style={{ marginBottom: 16 }} bodyStyle={{ padding: 16 }}>
        <Space wrap align="center">
          <RadioGroup type="button" value={preset} onChange={(e) => setPreset(e.target.value)}>
            {rangeOptions.map((o) => (
              <Radio key={o.value} value={o.value}>
                {o.label}
              </Radio>
            ))}
          </RadioGroup>

          {preset === 'custom' ? (
            <DatePicker
              type="dateTimeRange"
              value={customRange}
              onChange={(v) => v && setCustomRange(v)}
              style={{ width: 360 }}
            />
          ) : null}

          {isAdmin ? (
            <Select
              placeholder={t('filters.channel')}
              value={channelId}
              onChange={setChannelId}
              optionList={channelOptions}
              style={{ width: 160 }}
              showClear
              filter
            />
          ) : null}

          <Select
            placeholder={t('filters.model')}
            value={model}
            onChange={setModel}
            optionList={[]}
            style={{ width: 200 }}
            showClear
            filter
            allowCreate
            emptyContent={t('filters.noModels')}
          />

          <Select
            placeholder={t('filters.status')}
            value={status}
            onChange={setStatus}
            optionList={statusOptions}
            style={{ width: 140 }}
          />

          {isAdmin ? (
            <Select
              placeholder={t('filters.user')}
              value={userId}
              onChange={setUserId}
              optionList={userOptions}
              style={{ width: 160 }}
              showClear
              filter
            />
          ) : null}

          <Space align="center">
            <Text type="tertiary" size="small">
              {t('type.label')}
            </Text>
            <Select
              value={logType}
              onChange={setLogType}
              optionList={logTypeOptions}
              style={{ width: 120 }}
            />
          </Space>

          <Button theme="borderless" onClick={resetFilters}>
            {t('actions.resetFilters')}
          </Button>
        </Space>
        {!isAdmin ? (
          <div style={{ marginTop: 8 }}>
            <Text type="tertiary" size="small">
              {t('scope.ownUsage')}
            </Text>
          </div>
        ) : null}
      </Card>

      {/* Detail log table. The ref'd div is the flex child we measure; Semi's
          Card is a class component (no DOM ref), so the wrapper owns the layout. */}
      <div ref={tableCardRef} className="lg-table-card">
        <Card
          className="lg-card"
          bodyStyle={{ padding: 0 }}
          style={{ overflow: 'hidden', height: '100%' }}
        >
          <Table
            columns={columns}
            dataSource={logs}
            loading={logLoading}
            rowKey={(r) => r.id ?? `${r.created_at}-${r.model}-${Math.random()}`}
            scroll={{ x: 'max-content', y: scrollY }}
            size="middle"
            empty={t('empty.noData')}
            pagination={{
              currentPage: logPage,
              pageSize: PAGE_SIZE,
              total: logTotal,
              onPageChange: (p) => loadLogs(p),
            }}
          />
        </Card>
      </div>
    </div>
  );
}
