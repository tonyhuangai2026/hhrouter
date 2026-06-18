import React, { useState, useEffect, useCallback, useMemo } from 'react';
import {
  Typography,
  Space,
  Button,
  Card,
  Select,
  Table,
  Input,
  Tag,
  Toast,
  Popconfirm,
} from '@douyinfe/semi-ui';
import { IconRefresh, IconSave, IconDelete } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import { mapApiError } from '../api/helpers';
import { listChannels } from '../api/channels';
import { listPricing, upsertPricing, deletePricing } from '../api/pricing';
import { usdToMicro, microToUSD } from '../utils/money';

const { Title, Text } = Typography;

// Decode a channel's models field (JSONB array or already-parsed array) into a
// string list. The channels API returns `models` as an array already.
function channelModels(ch) {
  if (!ch) return [];
  const m = ch.models ?? ch.Models;
  if (Array.isArray(m)) return m;
  if (typeof m === 'string') {
    try {
      const parsed = JSON.parse(m);
      return Array.isArray(parsed) ? parsed : [];
    } catch {
      return [];
    }
  }
  return [];
}

// ---------------------------------------------------------------------------
// Admin-only model-pricing page. The /pricing route is guarded by
// ProtectedRoute adminOnly in App.jsx. Admins pick a channel, then set per-model
// input/output/cache-read/cache-write prices in USD per 1M tokens. Values are
// stored as micro-USD ints (the form is the single rounding source).
// ---------------------------------------------------------------------------
export default function Pricing() {
  const { t } = useTranslation(['pricing', 'common']);

  const [channels, setChannels] = useState([]);
  const [channelId, setChannelId] = useState(undefined);
  const [loading, setLoading] = useState(false);
  // rows: { [model]: { id, input, output, cacheRead, cacheWrite } } — price
  // fields are USD strings/numbers for the inputs; id is the existing row id (or
  // undefined when no price is configured yet).
  const [rows, setRows] = useState({});
  const [savingModel, setSavingModel] = useState(null);

  // Load channels once.
  useEffect(() => {
    let active = true;
    (async () => {
      try {
        const { items } = await listChannels({ page: 1, page_size: 200 });
        if (active) setChannels(items || []);
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.loadFailed'));
      }
    })();
    return () => {
      active = false;
    };
  }, [t]);

  const selectedChannel = useMemo(
    () => channels.find((c) => c.id === channelId),
    [channels, channelId]
  );
  const models = useMemo(() => channelModels(selectedChannel), [selectedChannel]);

  // Load existing prices for the selected channel and seed the row state from
  // the channel's model list (so unpriced models still get an editable row).
  const loadPrices = useCallback(
    async (cid, modelList) => {
      if (cid == null) return;
      setLoading(true);
      try {
        const { items } = await listPricing(cid);
        const byModel = {};
        for (const p of items || []) {
          byModel[p.model] = {
            id: p.id,
            input: microToUSD(p.input_micro_usd_per_m),
            output: microToUSD(p.output_micro_usd_per_m),
            cacheRead: microToUSD(p.cache_read_micro_usd_per_m),
            cacheWrite: microToUSD(p.cache_write_micro_usd_per_m),
          };
        }
        // Ensure every channel model has a row (blank if unpriced).
        const seeded = {};
        for (const m of modelList) {
          seeded[m] = byModel[m] || { id: undefined, input: '', output: '', cacheRead: '', cacheWrite: '' };
        }
        setRows(seeded);
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.loadFailed'));
        setRows({});
      } finally {
        setLoading(false);
      }
    },
    [t]
  );

  useEffect(() => {
    if (channelId != null) loadPrices(channelId, models);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channelId, channels]);

  const setField = useCallback((model, field, value) => {
    setRows((prev) => ({ ...prev, [model]: { ...prev[model], [field]: value } }));
  }, []);

  const handleSave = useCallback(
    async (model) => {
      const r = rows[model] || {};
      const input = usdToMicro(r.input);
      const output = usdToMicro(r.output);
      if (input <= 0 || output <= 0) {
        Toast.error(t('toast.inputOutputRequired'));
        return;
      }
      setSavingModel(model);
      try {
        const saved = await upsertPricing({
          channel_id: channelId,
          model,
          input_micro_usd_per_m: input,
          output_micro_usd_per_m: output,
          cache_read_micro_usd_per_m: usdToMicro(r.cacheRead),
          cache_write_micro_usd_per_m: usdToMicro(r.cacheWrite),
        });
        // Record the row id so a subsequent delete works without a reload.
        setRows((prev) => ({ ...prev, [model]: { ...prev[model], id: saved?.id ?? prev[model]?.id } }));
        Toast.success(t('toast.saved'));
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.saveFailed'));
      } finally {
        setSavingModel(null);
      }
    },
    [rows, channelId, t]
  );

  const handleDelete = useCallback(
    async (model) => {
      const r = rows[model];
      if (!r || r.id == null) {
        // Nothing persisted: just clear the inputs locally.
        setField(model, 'input', '');
        setField(model, 'output', '');
        setField(model, 'cacheRead', '');
        setField(model, 'cacheWrite', '');
        return;
      }
      try {
        await deletePricing(r.id);
        setRows((prev) => ({
          ...prev,
          [model]: { id: undefined, input: '', output: '', cacheRead: '', cacheWrite: '' },
        }));
        Toast.success(t('toast.deleted'));
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.deleteFailed'));
      }
    },
    [rows, setField, t]
  );

  const priceInput = (model, field) => (
    <Input
      value={rows[model]?.[field] ?? ''}
      onChange={(v) => setField(model, field, v)}
      type="number"
      min={0}
      step={0.01}
      prefix="$"
      size="small"
      style={{ width: 120 }}
    />
  );

  const columns = useMemo(
    () => [
      {
        title: t('columns.model'),
        dataIndex: 'model',
        render: (m) => {
          const priced = rows[m]?.id != null;
          return (
            <Space>
              <Text strong style={{ fontFamily: 'var(--semi-font-family-mono, monospace)' }}>{m}</Text>
              <Tag size="small" color={priced ? 'green' : 'grey'} type="light">
                {priced ? t('priced') : t('unpriced')}
              </Tag>
            </Space>
          );
        },
      },
      { title: t('columns.input'), render: (_, r) => priceInput(r.model, 'input') },
      { title: t('columns.output'), render: (_, r) => priceInput(r.model, 'output') },
      { title: t('columns.cacheRead'), render: (_, r) => priceInput(r.model, 'cacheRead') },
      { title: t('columns.cacheWrite'), render: (_, r) => priceInput(r.model, 'cacheWrite') },
      {
        title: t('columns.actions'),
        render: (_, r) => (
          <Space>
            <Button
              icon={<IconSave />}
              size="small"
              theme="solid"
              loading={savingModel === r.model}
              onClick={() => handleSave(r.model)}
            >
              {t('actions.save')}
            </Button>
            <Popconfirm title={t('actions.delete')} onConfirm={() => handleDelete(r.model)}>
              <Button icon={<IconDelete />} size="small" type="danger" theme="borderless" />
            </Popconfirm>
          </Space>
        ),
      },
    ],
    // rows/savingModel drive cell rendering; t for labels.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rows, savingModel, t]
  );

  const dataSource = useMemo(() => models.map((m) => ({ key: m, model: m })), [models]);

  const channelOptions = useMemo(
    () => channels.map((c) => ({ label: c.name || `#${c.id}`, value: c.id })),
    [channels]
  );

  return (
    <div>
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 4 }}>
        <Title heading={2}>{t('title')}</Title>
        {channelId != null ? (
          <Button icon={<IconRefresh />} onClick={() => loadPrices(channelId, models)}>
            {t('actions.refresh')}
          </Button>
        ) : null}
      </Space>
      <Text type="tertiary" style={{ display: 'block', marginBottom: 16 }}>
        {t('subtitle')}
      </Text>

      <Card style={{ marginBottom: 16 }} bodyStyle={{ padding: 16 }}>
        <Space align="center">
          <Text strong>{t('filters.channel')}</Text>
          <Select
            placeholder={t('filters.channelPlaceholder')}
            value={channelId}
            onChange={setChannelId}
            optionList={channelOptions}
            style={{ width: 260 }}
            filter
            showClear
          />
        </Space>
        <div style={{ marginTop: 8 }}>
          <Text type="tertiary" size="small">{t('hint.unit')}</Text>
        </div>
      </Card>

      {channelId == null ? (
        <Card bodyStyle={{ padding: 24 }}>
          <Text type="tertiary">{t('hint.selectChannel')}</Text>
        </Card>
      ) : models.length === 0 ? (
        <Card bodyStyle={{ padding: 24 }}>
          <Text type="tertiary">{t('hint.noModels')}</Text>
        </Card>
      ) : (
        <Card bodyStyle={{ padding: 0 }}>
          <Table
            columns={columns}
            dataSource={dataSource}
            loading={loading}
            pagination={false}
            size="middle"
            scroll={{ x: 'max-content' }}
          />
        </Card>
      )}
    </div>
  );
}
