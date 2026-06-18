import React, { useState, useCallback, useMemo, useEffect } from 'react';
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
  Switch,
} from '@douyinfe/semi-ui';
import { IconPlus, IconRefresh } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import usePaginatedList from '../components/usePaginatedList';
import { mapApiError } from '../api/helpers';
import { listRules, createRule, updateRule, deleteRule } from '../api/rules';
import { listChannels } from '../api/channels';

const { Title, Text } = Typography;

// Sort rules by priority ascending (lower = matched first, Tech Design §5).
function byPriority(a, b) {
  return (a.priority ?? 0) - (b.priority ?? 0);
}

export default function RoutingRules() {
  const { t } = useTranslation(['rules', 'common']);
  const { items, total, page, pageSize, loading, load } = usePaginatedList(listRules);

  const [channels, setChannels] = useState([]);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null);
  const [submitting, setSubmitting] = useState(false);
  const [formApi, setFormApi] = useState(null);

  // Load all channels for the target-channel multi-select (large page size).
  useEffect(() => {
    let active = true;
    listChannels({ page: 1, page_size: 200 })
      .then(({ items: chs }) => {
        if (active) setChannels(chs);
      })
      .catch((e) => Toast.error(mapApiError(e) || t('toast.loadChannelsFailed')));
    return () => {
      active = false;
    };
  }, [t]);

  const channelOptions = useMemo(
    () =>
      channels.map((c) => ({
        label: `${c.name} (#${c.id}, ${c.type})`,
        value: c.id,
      })),
    [channels]
  );

  const channelName = useCallback(
    (id) => {
      const c = channels.find((x) => x.id === id);
      return c ? c.name : `#${id}`;
    },
    [channels]
  );

  const sortedItems = useMemo(() => [...items].sort(byPriority), [items]);

  const openCreate = useCallback(() => {
    setEditing(null);
    setModalOpen(true);
  }, []);

  const openEdit = useCallback((record) => {
    setEditing(record);
    setModalOpen(true);
  }, []);

  const initValues = useMemo(() => {
    const m = editing?.match || {};
    if (editing) {
      return {
        name: editing.name,
        priority: editing.priority ?? 0,
        enabled: editing.enabled ?? true,
        groups: m.groups || [],
        models: m.models || [],
        min_tokens: m.min_tokens ?? null,
        max_tokens: m.max_tokens ?? null,
        target_channel_ids: editing.target_channel_ids || [],
        target_group: editing.target_group || '',
      };
    }
    return {
      name: '',
      priority: 0,
      enabled: true,
      groups: [],
      models: [],
      min_tokens: null,
      max_tokens: null,
      target_channel_ids: [],
      target_group: '',
    };
  }, [editing]);

  const buildPayload = useCallback((values) => {
    const match = {};
    if (values.groups?.length) match.groups = values.groups;
    if (values.models?.length) match.models = values.models;
    if (values.min_tokens != null && values.min_tokens !== '') match.min_tokens = Number(values.min_tokens);
    if (values.max_tokens != null && values.max_tokens !== '') match.max_tokens = Number(values.max_tokens);
    return {
      name: values.name,
      priority: Number(values.priority) || 0,
      enabled: !!values.enabled,
      match,
      target_channel_ids: values.target_channel_ids || [],
      target_group: values.target_group || null,
    };
  }, []);

  const handleSubmit = useCallback(async () => {
    if (!formApi) return;
    let values;
    try {
      values = await formApi.validate();
    } catch {
      return;
    }
    if (
      (!values.target_channel_ids || values.target_channel_ids.length === 0) &&
      !values.target_group
    ) {
      Toast.error(t('validation.targetRequired'));
      return;
    }
    const payload = buildPayload(values);
    setSubmitting(true);
    try {
      if (editing) {
        await updateRule(editing.id, payload);
        Toast.success(t('toast.updated'));
      } else {
        await createRule(payload);
        Toast.success(t('toast.created'));
      }
      setModalOpen(false);
      load(editing ? page : 1);
    } catch (e) {
      Toast.error(mapApiError(e) || t('toast.saveFailed'));
    } finally {
      setSubmitting(false);
    }
  }, [formApi, editing, buildPayload, load, page, t]);

  const handleDelete = useCallback(
    async (record) => {
      try {
        await deleteRule(record.id);
        Toast.success(t('toast.deleted'));
        load(page);
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.deleteFailed'));
      }
    },
    [load, page, t]
  );

  const handleToggle = useCallback(
    async (record, enabled) => {
      try {
        await updateRule(record.id, { enabled });
        Toast.success(enabled ? t('toast.enabled') : t('toast.disabled'));
        load(page);
      } catch (e) {
        Toast.error(mapApiError(e) || t('toast.updateFailed'));
      }
    },
    [load, page, t]
  );

  const renderMatch = useCallback((match) => {
    const m = match || {};
    const parts = [];
    if (m.groups?.length) parts.push(t('match.groups', { value: m.groups.join(', ') }));
    if (m.models?.length) parts.push(t('match.models', { value: m.models.join(', ') }));
    if (m.min_tokens != null) parts.push(t('match.min', { value: m.min_tokens }));
    if (m.max_tokens != null) parts.push(t('match.max', { value: m.max_tokens }));
    if (!parts.length) return <Text type="tertiary">{t('match.any')}</Text>;
    return (
      <Space spacing={4} wrap>
        {parts.map((p) => (
          <Tag key={p} size="small">
            {p}
          </Tag>
        ))}
      </Space>
    );
  }, [t]);

  const columns = useMemo(
    () => [
      { title: t('columns.priority'), dataIndex: 'priority', width: 90, sorter: byPriority, defaultSortOrder: 'ascend' },
      { title: t('columns.name'), dataIndex: 'name', width: 160 },
      { title: t('columns.match'), dataIndex: 'match', render: renderMatch },
      {
        title: t('columns.targetChannels'),
        dataIndex: 'target_channel_ids',
        render: (ids, record) => {
          const list = ids || [];
          if (!list.length && record.target_group) {
            return <Tag color="violet" size="small">{t('target.group', { value: record.target_group })}</Tag>;
          }
          if (!list.length) return <Text type="tertiary">{t('target.none')}</Text>;
          return (
            <Space spacing={4} wrap>
              {list.map((id) => (
                <Tag key={id} color="blue" size="small">
                  {channelName(id)}
                </Tag>
              ))}
            </Space>
          );
        },
      },
      {
        title: t('columns.enabled'),
        dataIndex: 'enabled',
        width: 90,
        render: (enabled, record) => (
          <Switch checked={!!enabled} onChange={(v) => handleToggle(record, v)} />
        ),
      },
      {
        title: t('common:labels.actions'),
        width: 160,
        render: (_, record) => (
          <Space>
            <Button size="small" theme="borderless" onClick={() => openEdit(record)}>
              {t('common:actions.edit')}
            </Button>
            <Popconfirm title={t('popconfirm.deleteTitle')} onConfirm={() => handleDelete(record)}>
              <Button size="small" theme="borderless" type="danger">
                {t('common:actions.delete')}
              </Button>
            </Popconfirm>
          </Space>
        ),
      },
    ],
    [renderMatch, channelName, handleToggle, openEdit, handleDelete, t]
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
            {t('actions.newRule')}
          </Button>
        </Space>
      </Space>

      <Table
        columns={columns}
        dataSource={sortedItems}
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
        title={editing ? t('actions.editRule') : t('actions.createRule')}
        visible={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleSubmit}
        confirmLoading={submitting}
        okText={editing ? t('common:actions.save') : t('common:actions.create')}
        maskClosable={false}
        width={600}
      >
        <Form initValues={initValues} getFormApi={setFormApi}>
          <Form.Input
            field="name"
            label={t('form.name')}
            rules={[{ required: true, message: t('form.nameRequired') }]}
          />
          <Space>
            <Form.InputNumber field="priority" label={t('form.priority')} min={0} style={{ width: 220 }} />
            <Form.Switch field="enabled" label={t('form.enabled')} />
          </Space>

          <Title heading={6} style={{ marginTop: 8 }}>
            {t('form.matchSectionTitle')}
          </Title>
          <Form.TagInput field="groups" label={t('form.groups')} placeholder={t('form.groupsPlaceholder')} allowDuplicates={false} />
          <Form.TagInput
            field="models"
            label={t('form.models')}
            placeholder={t('form.modelsPlaceholder')}
            allowDuplicates={false}
          />
          <Space>
            <Form.InputNumber field="min_tokens" label={t('form.minTokens')} min={0} style={{ width: 200 }} />
            <Form.InputNumber field="max_tokens" label={t('form.maxTokens')} min={0} style={{ width: 200 }} />
          </Space>

          <Title heading={6} style={{ marginTop: 8 }}>
            {t('form.targetSectionTitle')}
          </Title>
          <Form.Select
            field="target_channel_ids"
            label={t('form.targetChannels')}
            multiple
            filter
            optionList={channelOptions}
            placeholder={t('form.targetChannelsPlaceholder')}
            style={{ width: '100%' }}
          />
          <Form.Input
            field="target_group"
            label={t('form.targetGroup')}
            placeholder={t('form.targetGroupPlaceholder')}
          />
        </Form>
      </Modal>
    </div>
  );
}
