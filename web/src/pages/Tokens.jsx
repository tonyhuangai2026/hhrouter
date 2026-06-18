import React, { useState, useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  Table,
  Button,
  Typography,
  Space,
  Tag,
  Modal,
  Form,
  Toast,
  Popconfirm,
  Banner,
  Input,
} from '@douyinfe/semi-ui';
import { IconPlus, IconRefresh, IconCopy } from '@douyinfe/semi-icons';
import usePaginatedList from '../components/usePaginatedList';
import { mapApiError } from '../api/helpers';
import { listTokens, createToken, updateToken, deleteToken } from '../api/tokens';

const { Title, Text } = Typography;

const STATUS_COLORS = { enabled: 'green', disabled: 'grey', expired: 'red' };

// Mask a token for display. Prefer a server-provided masked field, otherwise
// fall back to prefix+suffix of any available key-ish field.
function maskKey(record) {
  if (record.key_masked) return record.key_masked;
  if (record.masked_key) return record.masked_key;
  const raw = record.key || '';
  if (raw && raw.length > 10) {
    return `${raw.slice(0, 6)}...${raw.slice(-4)}`;
  }
  const prefix = record.key_prefix || 'sk-';
  const suffix = record.key_suffix || '';
  return suffix ? `${prefix}...${suffix}` : `${prefix}...`;
}

export default function Tokens() {
  const { t } = useTranslation(['tokens', 'common']);
  const navigate = useNavigate();
  const { items, total, page, pageSize, loading, load } = usePaginatedList(listTokens);

  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null);
  const [submitting, setSubmitting] = useState(false);
  const [formApi, setFormApi] = useState(null);

  // One-time full key reveal.
  const [revealedKey, setRevealedKey] = useState(null);

  const statusOptions = useMemo(
    () => [
      { label: t('common:status.enabled'), value: 'enabled' },
      { label: t('common:status.disabled'), value: 'disabled' },
    ],
    [t]
  );

  // Response output-format options. '' = follow the endpoint (no override).
  const outputFormatOptions = useMemo(
    () => [
      { label: t('form.outputFormat.endpointDefault'), value: '' },
      { label: 'OpenAI', value: 'openai' },
      { label: 'Anthropic (Claude Messages)', value: 'anthropic' },
      { label: 'Bedrock', value: 'bedrock' },
    ],
    [t]
  );

  const openCreate = useCallback(() => {
    setEditing(null);
    setModalOpen(true);
  }, []);

  const openEdit = useCallback((record) => {
    setEditing(record);
    setModalOpen(true);
  }, []);

  const initValues = useMemo(() => {
    if (editing) {
      return {
        name: editing.name,
        quota: editing.quota ?? -1,
        expired_at: editing.expired_at ? new Date(editing.expired_at) : null,
        group: editing.group || 'default',
        allowed_models: editing.allowed_models || [],
        status: editing.status || 'enabled',
        // '' = follow the endpoint (no override).
        output_format: editing.output_format || '',
      };
    }
    return {
      name: '',
      quota: -1,
      expired_at: null,
      group: 'default',
      allowed_models: [],
      status: 'enabled',
      output_format: '',
    };
  }, [editing]);

  const handleSubmit = useCallback(async () => {
    if (!formApi) return;
    let values;
    try {
      values = await formApi.validate();
    } catch {
      return;
    }
    const payload = {
      ...values,
      // Serialize date to ISO (or null = no expiry).
      expired_at: values.expired_at ? new Date(values.expired_at).toISOString() : null,
    };
    setSubmitting(true);
    try {
      if (editing) {
        await updateToken(editing.id, payload);
        Toast.success(t('messages.updated'));
        setModalOpen(false);
        load(page);
      } else {
        const created = await createToken(payload);
        // Full plaintext key is returned exactly once.
        const fullKey = created?.key || created?.token || created?.secret || null;
        setModalOpen(false);
        if (fullKey) {
          setRevealedKey(fullKey);
        } else {
          Toast.warning(t('messages.createdNoKey'));
        }
        load(1);
      }
    } catch (e) {
      Toast.error(mapApiError(e));
    } finally {
      setSubmitting(false);
    }
  }, [formApi, editing, load, page, t]);

  const handleDelete = useCallback(
    async (record) => {
      try {
        await deleteToken(record.id);
        Toast.success(t('messages.deleted'));
        load(page);
      } catch (e) {
        Toast.error(mapApiError(e));
      }
    },
    [load, page, t]
  );

  const handleToggleStatus = useCallback(
    async (record) => {
      const next = record.status === 'enabled' ? 'disabled' : 'enabled';
      try {
        await updateToken(record.id, { status: next });
        Toast.success(next === 'enabled' ? t('messages.enabled') : t('messages.disabled'));
        load(page);
      } catch (e) {
        Toast.error(mapApiError(e));
      }
    },
    [load, page, t]
  );

  const copyKey = useCallback(
    async (value) => {
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(value);
          Toast.success(t('messages.copied'));
          return;
        }
        throw new Error('clipboard unavailable');
      } catch {
        Toast.warning(t('messages.copyManual'));
      }
    },
    [t]
  );

  const columns = useMemo(
    () => [
      { title: t('columns.name'), dataIndex: 'name', width: 160 },
      {
        title: t('columns.key'),
        width: 200,
        render: (_, record) => <Text code>{maskKey(record)}</Text>,
      },
      {
        title: t('columns.quota'),
        dataIndex: 'quota',
        width: 140,
        render: (q, record) => {
          if (q === -1 || q == null) return <Tag color="blue">{t('display.unlimited')}</Tag>;
          return (
            <Text>
              {record.used_quota ?? 0} / {q}
            </Text>
          );
        },
      },
      {
        title: t('columns.group'),
        dataIndex: 'group',
        width: 100,
        render: (g) => g || 'default',
      },
      {
        title: t('columns.expires'),
        dataIndex: 'expired_at',
        width: 160,
        render: (d) =>
          d ? new Date(d).toLocaleString() : <Text type="tertiary">{t('display.never')}</Text>,
      },
      {
        title: t('common:labels.status'),
        dataIndex: 'status',
        width: 100,
        render: (s) => (
          <Tag color={STATUS_COLORS[s] || 'grey'}>
            {s === 'enabled' || s === 'disabled' ? t(`common:status.${s}`) : s}
          </Tag>
        ),
      },
      {
        title: t('common:labels.actions'),
        width: 340,
        render: (_, record) => (
          <Space>
            <Button
              size="small"
              onClick={() => handleToggleStatus(record)}
              disabled={record.status === 'expired'}
            >
              {record.status === 'enabled' ? t('actions.disable') : t('actions.enable')}
            </Button>
            <Button
              size="small"
              theme="borderless"
              onClick={() => navigate(`/tokens/${record.id}/usage`)}
            >
              {t('actions.usage')}
            </Button>
            <Button size="small" theme="borderless" onClick={() => openEdit(record)}>
              {t('common:actions.edit')}
            </Button>
            <Popconfirm title={t('popconfirm.delete')} onConfirm={() => handleDelete(record)}>
              <Button size="small" theme="borderless" type="danger">
                {t('common:actions.delete')}
              </Button>
            </Popconfirm>
          </Space>
        ),
      },
    ],
    [t, handleToggleStatus, openEdit, handleDelete, navigate]
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
        empty={t('common:state.noData')}
        pagination={{
          currentPage: page,
          pageSize,
          total,
          onPageChange: (p) => load(p),
        }}
      />

      <Modal
        title={editing ? t('form.editTitle') : t('form.createTitle')}
        visible={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleSubmit}
        confirmLoading={submitting}
        okText={editing ? t('common:actions.save') : t('common:actions.create')}
        cancelText={t('common:actions.cancel')}
        maskClosable={false}
        width={520}
      >
        <Form initValues={initValues} getFormApi={setFormApi}>
          <Form.Input
            field="name"
            label={t('form.name.label')}
            rules={[{ required: true, message: t('form.name.required') }]}
          />
          <Form.InputNumber
            field="quota"
            label={t('form.quota.label')}
            min={-1}
            style={{ width: '100%' }}
          />
          <Form.DatePicker
            field="expired_at"
            label={t('form.expiredAt.label')}
            type="dateTime"
            style={{ width: '100%' }}
          />
          <Form.Input
            field="group"
            label={t('form.group.label')}
            placeholder={t('form.group.placeholder')}
          />
          <Form.TagInput
            field="allowed_models"
            label={t('form.allowedModels.label')}
            placeholder={t('form.allowedModels.placeholder')}
            allowDuplicates={false}
          />
          <Form.Select
            field="status"
            label={t('form.status.label')}
            optionList={statusOptions}
            style={{ width: '100%' }}
          />
          <Form.Select
            field="output_format"
            label={t('form.outputFormat.label')}
            optionList={outputFormatOptions}
            helpText={t('form.outputFormat.help')}
            style={{ width: '100%' }}
          />
        </Form>
      </Modal>

      {/* One-time full-key reveal dialog. */}
      <Modal
        title={t('reveal.title')}
        visible={!!revealedKey}
        onCancel={() => setRevealedKey(null)}
        footer={
          <Button theme="solid" type="primary" onClick={() => setRevealedKey(null)}>
            {t('common:actions.close')}
          </Button>
        }
        maskClosable={false}
        width={520}
      >
        <Banner type="warning" description={t('reveal.warning')} style={{ marginBottom: 16 }} />
        <Space style={{ width: '100%' }}>
          <Input value={revealedKey || ''} readonly style={{ flex: 1, fontFamily: 'monospace' }} />
          <Button icon={<IconCopy />} onClick={() => copyKey(revealedKey)}>
            {t('common:actions.copy')}
          </Button>
        </Space>
      </Modal>
    </div>
  );
}
