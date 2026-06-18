import React, { useState, useCallback, useMemo, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Table,
  Button,
  Typography,
  Space,
  Tag,
  Modal,
  Form,
  Input,
  Toast,
  Popconfirm,
} from '@douyinfe/semi-ui';
import { IconPlus, IconRefresh, IconDelete, IconCopy } from '@douyinfe/semi-icons';
import usePaginatedList from '../components/usePaginatedList';
import { mapApiError } from '../api/helpers';
import {
  listChannels,
  createChannel,
  updateChannel,
  deleteChannel,
  fetchModels,
  testChannel,
} from '../api/channels';

const { Title, Text } = Typography;

const STATUS_COLORS = {
  enabled: 'green',
  disabled: 'grey',
  auto_disabled: 'orange',
};

// Built-in Bedrock model suggestions (Tech Design §7); the field is free-form
// so admins can add/remove entries.
const BEDROCK_MODEL_SUGGESTIONS = [
  'anthropic.claude-3-5-sonnet-20240620-v1:0',
  'us.anthropic.claude-sonnet-4-20250514-v1:0',
  'amazon.nova-pro-v1:0',
  'meta.llama3-70b-instruct-v1:0',
];

// Common AWS regions where Bedrock is available. The field is free-form
// (allowCreate) so any other region — incl. gov/cn partitions — can be typed.
const AWS_REGION_OPTIONS = [
  { value: 'us-east-1', label: 'us-east-1 · N. Virginia' },
  { value: 'us-west-2', label: 'us-west-2 · Oregon' },
  { value: 'us-east-2', label: 'us-east-2 · Ohio' },
  { value: 'ap-northeast-1', label: 'ap-northeast-1 · Tokyo' },
  { value: 'ap-southeast-1', label: 'ap-southeast-1 · Singapore' },
  { value: 'ap-southeast-2', label: 'ap-southeast-2 · Sydney' },
  { value: 'ap-south-1', label: 'ap-south-1 · Mumbai' },
  { value: 'eu-central-1', label: 'eu-central-1 · Frankfurt' },
  { value: 'eu-west-1', label: 'eu-west-1 · Ireland' },
  { value: 'eu-west-3', label: 'eu-west-3 · Paris' },
];

export default function Channels() {
  const { t } = useTranslation(['channels', 'common']);
  const { items, total, page, pageSize, loading, load } = usePaginatedList(listChannels);

  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null); // null = create
  const [submitting, setSubmitting] = useState(false);
  const [formApi, setFormApi] = useState(null);
  const [formType, setFormType] = useState('openai');
  const [busyId, setBusyId] = useState(null); // row-level fetch/test spinner
  // model_mapping editor rows {id, key, value}. Kept in LOCAL state (NOT a Form
  // field) and merged into the payload in handleSubmit — this deliberately
  // avoids touching the form's onValueChange, which must never call
  // formApi.setValue (prior "too much recursion" regression). Each row carries
  // a stable `id` used as the React key so removing a middle row doesn't
  // mis-reconcile the remaining inputs' focus/value.
  const [mappingRows, setMappingRows] = useState([]);
  const rowIdRef = useRef(0);
  const nextRowId = useCallback(() => {
    rowIdRef.current += 1;
    return rowIdRef.current;
  }, []);
  // Live snapshot of the models field, kept in sync via the form's
  // onValueChange (setState is safe here — only setValue inside onValueChange
  // causes the recursion). Used to render click-to-copy model-name chips that
  // make it easy to fill the mapping rows.
  const [modelList, setModelList] = useState([]);

  // Copy a string to the clipboard with a "copied" toast (manual-copy fallback).
  const copyText = useCallback(
    async (value) => {
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(value);
          Toast.success(t('toast.copied', { value }));
          return;
        }
        throw new Error('clipboard unavailable');
      } catch {
        Toast.warning(t('toast.copyManual'));
      }
    },
    [t]
  );

  const TYPE_OPTIONS = useMemo(
    () => [
      { label: t('type.openai'), value: 'openai' },
      { label: t('type.bedrock'), value: 'bedrock' },
      { label: t('type.anthropic'), value: 'anthropic' },
    ],
    [t]
  );

  const STATUS_OPTIONS = useMemo(
    () => [
      { label: t('status.enabled'), value: 'enabled' },
      { label: t('status.disabled'), value: 'disabled' },
      { label: t('status.auto_disabled'), value: 'auto_disabled' },
    ],
    [t]
  );

  const openCreate = useCallback(() => {
    setEditing(null);
    setFormType('openai');
    setMappingRows([]);
    setModelList([]);
    setModalOpen(true);
  }, []);

  const openEdit = useCallback((record) => {
    setEditing(record);
    setFormType(record.type || 'openai');
    // Seed the mapping editor from the channel's stored model_mapping object.
    const mm = record.model_mapping || {};
    setMappingRows(
      Object.entries(mm).map(([key, value]) => ({
        id: nextRowId(),
        key,
        value: String(value ?? ''),
      }))
    );
    setModelList(record.models || []);
    setModalOpen(true);
  }, [nextRowId]);

  const initValues = useMemo(() => {
    if (editing) {
      return {
        name: editing.name,
        type: editing.type || 'openai',
        base_url: editing.base_url || '',
        region: editing.region || '',
        key: '', // never prefilled — backend stores encrypted/masked
        models: editing.models || [],
        use_inference_profile: editing.use_inference_profile ?? true,
        group: editing.group || 'default',
        priority: editing.priority ?? 0,
        weight: editing.weight ?? 1,
        status: editing.status || 'enabled',
      };
    }
    return {
      name: '',
      type: 'openai',
      base_url: 'https://api.openai.com',
      region: '',
      key: '',
      models: [],
      use_inference_profile: true,
      group: 'default',
      priority: 0,
      weight: 1,
      status: 'enabled',
    };
  }, [editing]);

  // ---- model_mapping editor row handlers (local state) ----
  const addMappingRow = useCallback(() => {
    setMappingRows((rows) => [...rows, { id: nextRowId(), key: '', value: '' }]);
  }, [nextRowId]);
  const updateMappingRow = useCallback((idx, field, val) => {
    setMappingRows((rows) => rows.map((r, i) => (i === idx ? { ...r, [field]: val } : r)));
  }, []);
  const removeMappingRow = useCallback((idx) => {
    setMappingRows((rows) => rows.filter((_, i) => i !== idx));
  }, []);

  // Collapse the editor rows into a {external: upstream} object: trim both
  // sides, drop rows with an empty key or value, last-wins on duplicate keys.
  const buildModelMapping = useCallback(() => {
    const out = {};
    for (const r of mappingRows) {
      const k = (r.key || '').trim();
      const v = (r.value || '').trim();
      if (k && v) out[k] = v;
    }
    return out;
  }, [mappingRows]);

  const handleSubmit = useCallback(async () => {
    if (!formApi) return;
    let values;
    try {
      values = await formApi.validate();
    } catch {
      return; // validation errors are shown inline
    }
    // Drop empty key on edit so we don't overwrite the stored secret.
    const payload = { ...values };
    if (editing && !payload.key) delete payload.key;
    // Strip the field that doesn't belong to the selected channel type so a
    // stale value (e.g. the default base_url left over from switching type)
    // never reaches the backend.
    if (payload.type === 'bedrock') {
      delete payload.base_url;
    } else {
      delete payload.region;
    }
    // For a NEW anthropic channel, default base_url to the Anthropic API when
    // the field is empty or still carries the openai create-default. Done at
    // submit time (not via setValue-in-onValueChange, which recurses) so the
    // correct default reaches the backend while the field stays user-editable.
    if (
      !editing &&
      payload.type === 'anthropic' &&
      (!payload.base_url || payload.base_url === 'https://api.openai.com')
    ) {
      payload.base_url = 'https://api.anthropic.com';
    }
    // Always send model_mapping (possibly {}) — the backend only writes it when
    // the field is present, so an empty object is how we clear a mapping on
    // edit. Applies to both channel types.
    payload.model_mapping = buildModelMapping();

    setSubmitting(true);
    try {
      if (editing) {
        await updateChannel(editing.id, payload);
        Toast.success(t('toast.updated'));
      } else {
        await createChannel(payload);
        Toast.success(t('toast.created'));
      }
      setModalOpen(false);
      load(editing ? page : 1);
    } catch (e) {
      Toast.error(mapApiError(e) || t('toast.saveFailed'));
    } finally {
      setSubmitting(false);
    }
  }, [formApi, editing, load, page, t, buildModelMapping]);

  const handleDelete = useCallback(
    async (record) => {
      try {
        await deleteChannel(record.id);
        Toast.success(t('toast.deleted'));
        load(page);
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.deleteFailed'));
      }
    },
    [load, page, t]
  );

  const handleFetchModels = useCallback(
    async (record) => {
      setBusyId(record.id);
      try {
        const res = await fetchModels(record.id);
        const models = res?.models || res?.data || [];
        Toast.success(
          t('toast.fetchModelsSuccess', {
            count: Array.isArray(models) ? models.length : 0,
            name: record.name,
          })
        );
        load(page);
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.fetchModelsFailed'));
      } finally {
        setBusyId(null);
      }
    },
    [load, page, t]
  );

  const handleTest = useCallback(
    async (record) => {
      setBusyId(record.id);
      try {
        const res = await testChannel(record.id);
        const ok = res?.success !== false; // treat absence of explicit failure as success
        const detail = res?.message || (res?.latency_ms != null ? `${res.latency_ms}ms` : '');
        if (ok) {
          Toast.success(
            detail
              ? t('toast.testPassedDetail', { name: record.name, detail })
              : t('toast.testPassed', { name: record.name })
          );
        } else {
          Toast.error(
            res?.message
              ? t('toast.testFailedDetail', { name: record.name, detail: res.message })
              : t('toast.testFailed', { name: record.name })
          );
        }
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.testError'));
      } finally {
        setBusyId(null);
      }
    },
    [t]
  );

  const columns = useMemo(
    () => [
      { title: t('columns.name'), dataIndex: 'name', width: 160 },
      {
        title: t('columns.type'),
        dataIndex: 'type',
        width: 100,
        render: (ty) => (
          <Tag color={ty === 'bedrock' ? 'violet' : 'blue'}>
            {t(`type.${ty}`, { defaultValue: ty })}
          </Tag>
        ),
      },
      { title: t('columns.group'), dataIndex: 'group', width: 100, render: (g) => g || 'default' },
      {
        title: t('columns.models'),
        dataIndex: 'models',
        render: (models) => {
          const list = models || [];
          if (!list.length) return <Text type="tertiary">{t('models.none')}</Text>;
          return (
            <Space spacing={4} wrap>
              {list.slice(0, 4).map((m) => (
                <Tag key={m} size="small">
                  {m}
                </Tag>
              ))}
              {list.length > 4 ? (
                <Text type="tertiary">{t('models.more', { count: list.length - 4 })}</Text>
              ) : null}
            </Space>
          );
        },
      },
      { title: t('columns.priority'), dataIndex: 'priority', width: 80 },
      { title: t('columns.weight'), dataIndex: 'weight', width: 80 },
      {
        title: t('columns.status'),
        dataIndex: 'status',
        width: 120,
        render: (s) => (
          <Tag color={STATUS_COLORS[s] || 'grey'}>{t(`status.${s}`, { defaultValue: s })}</Tag>
        ),
      },
      {
        title: t('columns.actions'),
        width: 320,
        render: (_, record) => (
          <Space>
            <Button
              size="small"
              loading={busyId === record.id}
              onClick={() => handleFetchModels(record)}
            >
              {t('actions.fetchModels')}
            </Button>
            <Button
              size="small"
              loading={busyId === record.id}
              onClick={() => handleTest(record)}
            >
              {t('actions.test')}
            </Button>
            <Button size="small" theme="borderless" onClick={() => openEdit(record)}>
              {t('common:actions.edit')}
            </Button>
            <Popconfirm title={t('confirm.delete')} onConfirm={() => handleDelete(record)}>
              <Button size="small" theme="borderless" type="danger">
                {t('common:actions.delete')}
              </Button>
            </Popconfirm>
          </Space>
        ),
      },
    ],
    [busyId, handleFetchModels, handleTest, openEdit, handleDelete, t]
  );

  return (
    <div>
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 16 }}>
        <Title heading={2}>{t('title')}</Title>
        <Space>
          <Button icon={<IconRefresh />} onClick={() => load(page)}>
            {t('common:actions.refresh')}
          </Button>
          <Button theme="solid" type="primary" icon={<IconPlus />} onClick={openCreate}>
            {t('actions.new')}
          </Button>
        </Space>
      </Space>

      <Table
        columns={columns}
        dataSource={items}
        loading={loading}
        rowKey="id"
        pagination={{
          currentPage: page,
          pageSize,
          total,
          onPageChange: (p) => load(p),
        }}
      />

      <Modal
        title={editing ? t('modal.editTitle') : t('modal.createTitle')}
        visible={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleSubmit}
        confirmLoading={submitting}
        okText={editing ? t('common:actions.save') : t('common:actions.create')}
        cancelText={t('common:actions.cancel')}
        maskClosable={false}
        width={560}
      >
        <Form
          initValues={initValues}
          getFormApi={setFormApi}
          onValueChange={(vals) => {
            // Only track the selected type to swap which field is shown.
            // Do NOT call formApi.setValue() here: setValue re-triggers
            // onValueChange, and since formType (React state) doesn't update
            // synchronously, the guard below stays true and recurses infinitely
            // ("too much recursion"). Field isolation is handled instead by the
            // distinct key + component type on each branch, and by stripping the
            // irrelevant field in handleSubmit.
            if (vals.type && vals.type !== formType) setFormType(vals.type);
            // Mirror the models field into local state for the copy chips.
            // This is setState (safe), NOT formApi.setValue (which would recurse).
            setModelList(Array.isArray(vals.models) ? vals.models : []);
          }}
        >
          <Form.Input
            field="name"
            label={t('form.name')}
            rules={[{ required: true, message: t('form.nameRequired') }]}
          />
          <Form.Select
            field="type"
            label={t('form.type')}
            optionList={TYPE_OPTIONS}
            style={{ width: '100%' }}
            disabled={!!editing}
          />

          {formType === 'bedrock' ? (
            <Form.Select
              key="field-region"
              field="region"
              label={t('form.region')}
              placeholder={t('form.regionPlaceholder')}
              optionList={AWS_REGION_OPTIONS}
              filter
              allowCreate
              style={{ width: '100%' }}
              rules={[{ required: true, message: t('form.regionRequired') }]}
            />
          ) : (
            <Form.Input
              key="field-base_url"
              field="base_url"
              label={t('form.baseUrl')}
              placeholder={
                formType === 'anthropic'
                  ? 'https://api.anthropic.com'
                  : t('form.baseUrlPlaceholder')
              }
              rules={[{ required: true, message: t('form.baseUrlRequired') }]}
            />
          )}

          <Form.Input
            field="key"
            label={editing ? t('form.keyEdit') : t('form.key')}
            mode="password"
            placeholder={
              formType === 'bedrock' ? t('form.keyPlaceholderBedrock') : t('form.keyPlaceholderOpenai')
            }
          />

          <Form.TagInput
            field="models"
            label={t('form.models')}
            placeholder={t('form.modelsPlaceholder')}
            allowDuplicates={false}
          />
          {formType === 'bedrock' ? (
            <div style={{ marginTop: -8, marginBottom: 12 }}>
              <Text type="tertiary" size="small">
                {t('form.suggestions')}{' '}
                {BEDROCK_MODEL_SUGGESTIONS.map((m) => (
                  <Tag
                    key={m}
                    size="small"
                    style={{ cursor: 'pointer', marginRight: 4 }}
                    onClick={() => {
                      if (!formApi) return;
                      const cur = formApi.getValue('models') || [];
                      if (!cur.includes(m)) formApi.setValue('models', [...cur, m]);
                    }}
                  >
                    + {m}
                  </Tag>
                ))}
              </Text>
            </div>
          ) : null}
          {/* model_mapping editor — local-state rows, merged in handleSubmit.
              Bound to plain Inputs (NOT Form fields) on purpose. */}
          <Form.Slot label={t('form.modelMapping')}>
            <div>
              <Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 8 }}>
                {t('form.modelMappingServedNote')}
              </Text>
              {/* Click-to-copy chips of the channel's current model names, so
                  the user can quickly grab a name to paste into a mapping row. */}
              {modelList.length > 0 ? (
                <div style={{ marginBottom: 10 }}>
                  <Text type="tertiary" size="small" style={{ marginRight: 6 }}>
                    {t('form.modelMappingCopyHint')}
                  </Text>
                  <Space spacing={4} wrap>
                    {modelList.map((m) => (
                      <Tag
                        key={m}
                        size="small"
                        suffixIcon={<IconCopy />}
                        style={{ cursor: 'pointer' }}
                        onClick={() => copyText(m)}
                      >
                        {m}
                      </Tag>
                    ))}
                  </Space>
                </div>
              ) : null}
              {mappingRows.length === 0 ? (
                <Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 8 }}>
                  {t('form.modelMappingEmpty')}
                </Text>
              ) : null}
              {mappingRows.map((row, idx) => (
                <Space key={row.id} style={{ width: '100%', marginBottom: 8 }} align="center">
                  <Input
                    value={row.key}
                    onChange={(v) => updateMappingRow(idx, 'key', v)}
                    placeholder={t('form.modelMappingExternal')}
                    style={{ width: 200 }}
                  />
                  <Text type="tertiary">→</Text>
                  <Input
                    value={row.value}
                    onChange={(v) => updateMappingRow(idx, 'value', v)}
                    placeholder={t('form.modelMappingUpstream')}
                    style={{ width: 220 }}
                  />
                  <Button
                    theme="borderless"
                    type="tertiary"
                    icon={<IconDelete />}
                    aria-label={t('form.modelMappingRemove')}
                    onClick={() => removeMappingRow(idx)}
                  />
                </Space>
              ))}
              <Button theme="borderless" icon={<IconPlus />} onClick={addMappingRow}>
                {t('form.modelMappingAdd')}
              </Button>
            </div>
          </Form.Slot>

          {formType === 'bedrock' ? (
            <Form.Switch
              field="use_inference_profile"
              label={t('form.useInferenceProfile')}
              extraText={t('form.useInferenceProfileHelp')}
              initValue={true}
            />
          ) : null}

          <Form.Input field="group" label={t('form.group')} placeholder={t('form.groupPlaceholder')} />
          <Space>
            <Form.InputNumber field="priority" label={t('form.priority')} min={0} />
            <Form.InputNumber field="weight" label={t('form.weight')} min={1} />
          </Space>
          <Form.Select
            field="status"
            label={t('form.status')}
            optionList={STATUS_OPTIONS}
            style={{ width: '100%' }}
          />
        </Form>
      </Modal>
    </div>
  );
}
