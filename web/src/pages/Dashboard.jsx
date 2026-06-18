import React, { useState, useEffect, useCallback, useMemo, Suspense, lazy } from 'react';
import {
  Typography,
  Space,
  Button,
  Card,
  RadioGroup,
  Radio,
  DatePicker,
  Select,
  Spin,
  Toast,
  Empty,
} from '@douyinfe/semi-ui';
import { IconRefresh } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
// Lazy-load the chart component so ECharts is code-split out of the main
// bundle and only fetched when the Dashboard is opened.
const ReactECharts = lazy(() => import('echarts-for-react'));
import { useAuth } from '../context/AuthContext';
import { useTheme } from '../context/ThemeContext';
import { mapApiError } from '../api/helpers';
import { getSummary, getTimeseries } from '../api/dashboard';
import { listChannels } from '../api/channels';
import { listUsers } from '../api/users';
import { formatUSD } from '../utils/money';

const { Title, Text } = Typography;

// ---------------------------------------------------------------------------
// Time range presets. "custom" lets the user pick an explicit [start, end].
// Labels are resolved via t() inside the component (see RANGE_VALUES).
// ---------------------------------------------------------------------------
const RANGE_VALUES = ['today', '7d', '30d', 'custom'];

const DAY_MS = 24 * 60 * 60 * 1000;

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

// today/7d → hourly-ish vs daily buckets keeps the timeseries readable.
function intervalForRange(start, end) {
  return end.getTime() - start.getTime() <= 2 * DAY_MS ? 'hour' : 'day';
}

// ---------------------------------------------------------------------------
// Response normalization. T8 is not running yet and the Tech Design §8 only
// names the endpoints, so we accept several plausible shapes and degrade
// gracefully to zeros/empties rather than crashing the page.
// ---------------------------------------------------------------------------
function num(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

function normalizeSummary(raw) {
  const s = raw || {};
  const totalRequests = num(s.total_requests ?? s.totalRequests ?? s.requests ?? s.count);
  const successRequests = num(
    s.success_requests ?? s.successRequests ?? s.success ?? s.success_count
  );
  // success_rate may arrive as a fraction (0..1) or percent (0..100), or be
  // derivable from counts.
  let successRate;
  if (s.success_rate != null || s.successRate != null) {
    let r = num(s.success_rate ?? s.successRate);
    if (r <= 1) r *= 100;
    successRate = r;
  } else if (totalRequests > 0) {
    successRate = (successRequests / totalRequests) * 100;
  } else {
    successRate = 0;
  }
  const promptTokens = num(s.prompt_tokens ?? s.promptTokens);
  const completionTokens = num(s.completion_tokens ?? s.completionTokens);
  const totalTokens = num(
    s.total_tokens ?? s.totalTokens ?? promptTokens + completionTokens
  );
  const avgLatency = num(
    s.avg_latency_ms ?? s.avgLatencyMs ?? s.avg_latency ?? s.average_latency_ms
  );
  const costMicroUSD = num(s.cost_micro_usd ?? s.costMicroUSD);
  return {
    totalRequests,
    successRate,
    promptTokens,
    completionTokens,
    totalTokens,
    avgLatency,
    costMicroUSD,
  };
}

// Normalize a timeseries payload into:
//   { buckets: [ts,...], series: { [groupKey]: { requests:[], tokens:[] } } }
// Accepts either a flat array of rows or { points|data|series: [...] }.
function normalizeTimeseries(raw, groupBy) {
  const rows = Array.isArray(raw)
    ? raw
    : raw?.points || raw?.data || raw?.series || raw?.items || [];

  const bucketSet = [];
  const bucketIndex = new Map();
  const series = new Map();

  const ensureBucket = (ts) => {
    if (!bucketIndex.has(ts)) {
      bucketIndex.set(ts, bucketSet.length);
      bucketSet.push(ts);
    }
    return bucketIndex.get(ts);
  };

  // Shape A: array of grouped series objects { group, points:[{ts,requests,tokens}] }
  const looksGrouped =
    Array.isArray(rows) &&
    rows.length > 0 &&
    (rows[0].points || rows[0].data) &&
    (rows[0].group != null || rows[0].name != null || rows[0].key != null);

  if (looksGrouped) {
    rows.forEach((g) => {
      const key = String(g.group ?? g.name ?? g.key ?? 'all');
      const pts = g.points || g.data || [];
      const entry = { requests: [], tokens: [] };
      series.set(key, entry);
      pts.forEach((p) => {
        const ts = p.ts ?? p.time ?? p.bucket ?? p.created_at ?? p.date;
        const idx = ensureBucket(ts);
        entry.requests[idx] = num(p.requests ?? p.count ?? p.request_count);
        entry.tokens[idx] = num(p.tokens ?? p.total_tokens ?? p.totalTokens);
      });
    });
  } else if (Array.isArray(rows)) {
    // Shape B: flat rows [{ ts, requests, tokens, group? }]
    rows.forEach((p) => {
      const ts = p.ts ?? p.time ?? p.bucket ?? p.created_at ?? p.date;
      const idx = ensureBucket(ts);
      const key = String(
        (groupBy && (p[groupBy] ?? p.group)) ?? p.group ?? p.model ?? p.channel ?? 'all'
      );
      if (!series.has(key)) series.set(key, { requests: [], tokens: [] });
      const entry = series.get(key);
      entry.requests[idx] = num(p.requests ?? p.count ?? p.request_count);
      entry.tokens[idx] = num(p.tokens ?? p.total_tokens ?? p.totalTokens);
    });
  }

  // Fill holes with 0 so ECharts lines up with the bucket axis.
  const seriesObj = {};
  series.forEach((entry, key) => {
    seriesObj[key] = {
      requests: bucketSet.map((_, i) => entry.requests[i] || 0),
      tokens: bucketSet.map((_, i) => entry.tokens[i] || 0),
    };
  });

  return { buckets: bucketSet, series: seriesObj };
}

// Build pie-share data [{name, value}] from grouped timeseries (sum of tokens
// or requests per group). Falls back to an empty array.
function pieFromTimeseries(norm, metric) {
  return Object.entries(norm.series).map(([name, v]) => ({
    name,
    value: (v[metric] || []).reduce((a, b) => a + b, 0),
  }));
}

function fmtNum(n) {
  return Number(n || 0).toLocaleString();
}

function fmtBucket(ts) {
  if (ts == null) return '';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return String(ts);
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

// ---------------------------------------------------------------------------
// Stat card
// ---------------------------------------------------------------------------
function StatCard({ title, value, suffix, footer, loading }) {
  return (
    <Card style={{ flex: 1, minWidth: 200 }} bodyStyle={{ padding: 16 }}>
      <Text type="tertiary" size="small">
        {title}
      </Text>
      <div style={{ marginTop: 8, minHeight: 36 }}>
        {loading ? (
          <Spin size="small" />
        ) : (
          <Title heading={3} style={{ margin: 0 }}>
            {value}
            {suffix ? (
              <Text type="tertiary" size="small" style={{ marginLeft: 4 }}>
                {suffix}
              </Text>
            ) : null}
          </Title>
        )}
      </div>
      {footer ? (
        <div style={{ marginTop: 4 }}>
          <Text type="tertiary" size="small">
            {footer}
          </Text>
        </div>
      ) : null}
    </Card>
  );
}

export default function Dashboard() {
  const { t, i18n } = useTranslation(['dashboard', 'common']);
  const { isAdmin } = useAuth();
  const { isDark } = useTheme();
  // ECharts' bundled 'dark' theme paints an opaque dark canvas that clashes
  // with Semi cards, so we keep a transparent background and only adjust the
  // text/axis colors to match the active Semi theme.
  const axisColor = isDark ? '#3f3f46' : '#e5e7eb';
  const textColor = isDark ? 'rgba(255,255,255,0.85)' : 'rgba(0,0,0,0.75)';

  // Filters / controls.
  const [preset, setPreset] = useState('7d');
  const [customRange, setCustomRange] = useState(() => presetRange('7d'));
  const [groupBy, setGroupBy] = useState('model');
  const [channelId, setChannelId] = useState(undefined);
  const [model, setModel] = useState(undefined);
  const [status, setStatus] = useState('');
  const [userId, setUserId] = useState(undefined); // admin only

  // Data.
  const [summary, setSummary] = useState(null);
  const [timeseries, setTimeseries] = useState({ buckets: [], series: {} });
  const [summaryLoading, setSummaryLoading] = useState(false);
  const [chartLoading, setChartLoading] = useState(false);

  // Filter option sources (admin can list channels/users; normal users cannot).
  const [channelOptions, setChannelOptions] = useState([]);
  const [userOptions, setUserOptions] = useState([]);
  const [modelOptions, setModelOptions] = useState([]);

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
  const groupOptions = useMemo(
    () => [
      { value: 'channel', label: t('group.byChannel') },
      { value: 'model', label: t('group.byModel') },
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

  // Common filter params shared by summary/timeseries.
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

  const loadSummary = useCallback(async () => {
    setSummaryLoading(true);
    try {
      const raw = await getSummary(filterParams);
      setSummary(normalizeSummary(raw));
    } catch (e) {
      Toast.error(mapApiError(e) || t('errors.loadSummary'));
      setSummary(normalizeSummary(null));
    } finally {
      setSummaryLoading(false);
    }
  }, [filterParams, t]);

  const loadTimeseries = useCallback(async () => {
    setChartLoading(true);
    try {
      const [start, end] = range;
      const raw = await getTimeseries({
        ...filterParams,
        group_by: groupBy,
        interval: start && end ? intervalForRange(new Date(start), new Date(end)) : 'day',
      });
      const norm = normalizeTimeseries(raw, groupBy);
      setTimeseries(norm);
      // Derive model filter options from grouped data when grouping by model.
      if (groupBy === 'model') {
        const keys = Object.keys(norm.series).filter((k) => k && k !== 'all');
        if (keys.length) {
          setModelOptions(keys.map((k) => ({ label: k, value: k })));
        }
      }
    } catch (e) {
      Toast.error(mapApiError(e) || t('errors.loadTimeseries'));
      setTimeseries({ buckets: [], series: {} });
    } finally {
      setChartLoading(false);
    }
  }, [filterParams, groupBy, range, t]);

  // Reload summary + charts whenever the shared filters change.
  const filtersKey = JSON.stringify(filterParams) + '|' + groupBy;
  useEffect(() => {
    loadSummary();
    loadTimeseries();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filtersKey]);

  const refreshAll = useCallback(() => {
    loadSummary();
    loadTimeseries();
  }, [loadSummary, loadTimeseries]);

  // -------------------------------------------------------------------------
  // Chart options
  // -------------------------------------------------------------------------
  const seriesKeys = useMemo(() => Object.keys(timeseries.series), [timeseries]);
  const hasChartData = timeseries.buckets.length > 0 && seriesKeys.length > 0;

  const timeseriesOption = useMemo(() => {
    const xAxis = timeseries.buckets.map(fmtBucket);
    const requestSeries = seriesKeys.map((k) => ({
      name: t('charts.legend.requests', { name: k }),
      type: 'line',
      smooth: true,
      showSymbol: false,
      data: timeseries.series[k].requests,
    }));
    const tokenSeries = seriesKeys.map((k) => ({
      name: t('charts.legend.tokens', { name: k }),
      type: 'bar',
      yAxisIndex: 1,
      stack: 'tokens',
      data: timeseries.series[k].tokens,
    }));
    return {
      backgroundColor: 'transparent',
      textStyle: { color: textColor },
      tooltip: { trigger: 'axis' },
      legend: { type: 'scroll', top: 0, textStyle: { color: textColor } },
      grid: { left: 48, right: 48, top: 48, bottom: 40 },
      xAxis: {
        type: 'category',
        data: xAxis,
        boundaryGap: true,
        axisLine: { lineStyle: { color: axisColor } },
        axisLabel: { color: textColor },
      },
      yAxis: [
        {
          type: 'value',
          name: t('charts.axis.requests'),
          axisLabel: { color: textColor },
          splitLine: { lineStyle: { color: axisColor } },
        },
        {
          type: 'value',
          name: t('charts.axis.tokens'),
          splitLine: { show: false },
          axisLabel: { color: textColor },
        },
      ],
      series: [...tokenSeries, ...requestSeries],
    };
    // i18n.language is included so the chart option rebuilds on language change.
  }, [timeseries, seriesKeys, axisColor, textColor, t, i18n.language]);

  const pieData = useMemo(() => pieFromTimeseries(timeseries, 'tokens'), [timeseries]);
  const pieOption = useMemo(
    () => ({
      backgroundColor: 'transparent',
      textStyle: { color: textColor },
      tooltip: { trigger: 'item', formatter: '{b}: {c} ({d}%)' },
      legend: { type: 'scroll', bottom: 0, textStyle: { color: textColor } },
      series: [
        {
          name: groupBy === 'channel' ? t('charts.pie.channelShare') : t('charts.pie.modelShare'),
          type: 'pie',
          radius: ['40%', '70%'],
          avoidLabelOverlap: true,
          itemStyle: { borderRadius: 4, borderWidth: 1 },
          label: { formatter: '{b}\n{d}%', color: textColor },
          data: pieData,
        },
      ],
    }),
    // i18n.language is included so the chart option rebuilds on language change.
    [pieData, groupBy, textColor, t, i18n.language]
  );

  const resetFilters = useCallback(() => {
    setChannelId(undefined);
    setModel(undefined);
    setStatus('');
    setUserId(undefined);
  }, []);

  return (
    <div>
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 16 }}>
        <Title heading={2}>{t('title')}</Title>
        <Button icon={<IconRefresh />} onClick={refreshAll}>
          {t('actions.refresh')}
        </Button>
      </Space>

      {/* Time range + filters */}
      <Card style={{ marginBottom: 16 }} bodyStyle={{ padding: 16 }}>
        <Space wrap align="center">
          <RadioGroup
            type="button"
            value={preset}
            onChange={(e) => setPreset(e.target.value)}
          >
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
            optionList={modelOptions}
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

      {/* Stat cards */}
      <Space style={{ width: '100%', marginBottom: 16 }} wrap>
        <StatCard
          title={t('stats.totalRequests')}
          value={fmtNum(summary?.totalRequests)}
          loading={summaryLoading}
        />
        <StatCard
          title={t('stats.successRate')}
          value={`${(summary?.successRate ?? 0).toFixed(1)}`}
          suffix="%"
          loading={summaryLoading}
        />
        <StatCard
          title={t('stats.totalTokens')}
          value={fmtNum(summary?.totalTokens)}
          footer={t('stats.tokenSplit', {
            prompt: fmtNum(summary?.promptTokens),
            completion: fmtNum(summary?.completionTokens),
          })}
          loading={summaryLoading}
        />
        <StatCard
          title={t('stats.avgLatency')}
          value={fmtNum(summary?.avgLatency != null ? Math.round(summary.avgLatency) : 0)}
          suffix="ms"
          loading={summaryLoading}
        />
        <StatCard
          title={t('stats.totalCost')}
          value={formatUSD(summary?.costMicroUSD ?? 0)}
          loading={summaryLoading}
        />
      </Space>

      {/* Grouping control for charts */}
      <Space style={{ marginBottom: 12 }} align="center">
        <Text type="tertiary">{t('group.label')}</Text>
        <RadioGroup type="button" value={groupBy} onChange={(e) => setGroupBy(e.target.value)}>
          {groupOptions.map((o) => (
            <Radio key={o.value} value={o.value}>
              {o.label}
            </Radio>
          ))}
        </RadioGroup>
      </Space>

      {/* Charts */}
      <Space
        style={{ width: '100%', marginBottom: 16, alignItems: 'stretch' }}
        wrap
      >
        <Card
          title={t('charts.timeseriesTitle')}
          style={{ flex: 2, minWidth: 420 }}
          bodyStyle={{ padding: 8 }}
        >
          <Spin spinning={chartLoading}>
            {hasChartData ? (
              <Suspense fallback={<div style={{ height: 320 }} />}>
                <ReactECharts
                  option={timeseriesOption}
                  notMerge
                  style={{ height: 320 }}
                  opts={{ renderer: 'canvas' }}
                />
              </Suspense>
            ) : (
              <Empty description={t('empty.noData')} style={{ padding: 60 }} />
            )}
          </Spin>
        </Card>

        <Card
          title={groupBy === 'channel' ? t('charts.shareByChannel') : t('charts.shareByModel')}
          style={{ flex: 1, minWidth: 320 }}
          bodyStyle={{ padding: 8 }}
        >
          <Spin spinning={chartLoading}>
            {pieData.some((d) => d.value > 0) ? (
              <Suspense fallback={<div style={{ height: 320 }} />}>
                <ReactECharts
                  option={pieOption}
                  notMerge
                  style={{ height: 320 }}
                  opts={{ renderer: 'canvas' }}
                />
              </Suspense>
            ) : (
              <Empty description={t('empty.noData')} style={{ padding: 60 }} />
            )}
          </Spin>
        </Card>
      </Space>
    </div>
  );
}
