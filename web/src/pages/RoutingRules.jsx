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
  Tooltip,
  Card,
  Collapse,
  Input,
} from '@douyinfe/semi-ui';
import { IconPlus, IconRefresh } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import usePaginatedList from '../components/usePaginatedList';
import { mapApiError } from '../api/helpers';
import {
  listRules,
  createRule,
  updateRule,
  deleteRule,
  getRouterProbe,
  setRouterProbe,
  testRouterProbe,
  listRuleGroups,
} from '../api/rules';
import { listChannels } from '../api/channels';

const { Title, Text } = Typography;

// Clickable routing-expression examples shown under the expr field. The `expr`
// is the literal (language-neutral) expression inserted on click; `key` maps to
// a localized one-line description in rules.json (form.exprEx.<key>).
const EXPR_EXAMPLES = [
  { key: 'write', expr: 'w == 1' },
  { key: 'longWrite', expr: 'w == 1 && t > 500' },
  { key: 'short', expr: 't < 150' },
  { key: 'vipOrWrite', expr: 'group == "vip" || w == 1' },
  { key: 'bigPrompt', expr: 'tokens > 8000' },
  { key: 'modelAndWrite', expr: 'model == "gpt-4o" && w == 1' },
  // Difficulty tiering (Tech Design §5): the middle tier of an
  // opus / sonnet / haiku ladder, meant to be paired with a target model.
  { key: 'midTier', expr: 'w == 1 || t > 150' },
];

// Sort rules by priority ascending (lower = matched first, Tech Design §5).
function byPriority(a, b) {
  return (a.priority ?? 0) - (b.priority ?? 0);
}

export default function RoutingRules() {
  const { t } = useTranslation(['rules', 'common']);
  const { items, total, page, pageSize, loading, load } = usePaginatedList(listRules);

  const [channels, setChannels] = useState([]);
  const [groupOptions, setGroupOptions] = useState([]);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null);
  const [submitting, setSubmitting] = useState(false);
  const [formApi, setFormApi] = useState(null);

  // Distinct routing groups for the "key groups" dropdown (best-effort).
  useEffect(() => {
    listRuleGroups()
      .then((gs) => setGroupOptions((gs || []).map((g) => ({ label: g, value: g }))))
      .catch(() => {});
  }, []);

  // Routing-classifier (probe) settings.
  const [probe, setProbe] = useState({ mock: true, url: '', region: '' });
  const [probeSaving, setProbeSaving] = useState(false);
  useEffect(() => {
    getRouterProbe()
      .then((p) => setProbe({ mock: p.mock ?? true, url: p.url || '', region: p.region || '' }))
      .catch(() => {});
  }, []);

  const saveProbe = useCallback(async () => {
    setProbeSaving(true);
    try {
      const saved = await setRouterProbe(probe);
      setProbe({ mock: saved.mock ?? true, url: saved.url || '', region: saved.region || '' });
      Toast.success(t('probe.saved'));
    } catch (e) {
      Toast.error(mapApiError(e) || t('probe.saveFailed'));
    } finally {
      setProbeSaving(false);
    }
  }, [probe, t]);

  // Connectivity test: send one real classification request to the typed URL.
  const [probeTesting, setProbeTesting] = useState(false);
  const [probeTestResult, setProbeTestResult] = useState(null);
  const testProbe = useCallback(async () => {
    setProbeTesting(true);
    setProbeTestResult(null);
    try {
      const r = await testRouterProbe(probe.url);
      setProbeTestResult(r);
    } catch (e) {
      setProbeTestResult({ ok: false, error: mapApiError(e) || 'request failed' });
    } finally {
      setProbeTesting(false);
    }
  }, [probe.url]);

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

  // Live mirror of the form fields we must react to from outside the Form: the
  // target-model suggestions and the "model not served" soft warning depend on
  // target_channel_ids / target_group / target_model. Null until the first
  // edit, because Semi does not fire onValueChange for the initial render.
  //
  // Only the three fields we actually read are mirrored, and they are copied
  // out of Semi's values object: Semi hands onValueChange the SAME object every
  // time (it mutates its internal values in place), so storing that reference
  // directly would hit React's identity bail-out and never re-render.
  //
  // This is setState only — never call formApi.setValue() inside onValueChange
  // (it re-triggers onValueChange and recurses; see Channels.jsx).
  const [liveValues, setLiveValues] = useState(null);

  const mirrorValues = useCallback((vals) => {
    setLiveValues({
      target_channel_ids: [...(vals.target_channel_ids || [])],
      target_group: vals.target_group || '',
      target_model: vals.target_model || '',
    });
  }, []);

  const openCreate = useCallback(() => {
    setEditing(null);
    setLiveValues(null);
    setModalOpen(true);
  }, []);

  const openEdit = useCallback((record) => {
    setEditing(record);
    setLiveValues(null);
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
        // target_model is json:"...,omitempty" server-side, so the key is
        // absent (undefined) rather than "" when there is no override. Both
        // mean "no override" here. NOTE: this form-level initValues is the ONLY
        // place the saved value may be seeded — do NOT add a field-level
        // initValue on the target_model field, it would override this and show
        // the default instead of the saved value (the ab127e1 bug).
        target_model: editing.target_model || '',
        expr: editing.expr || '',
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
      target_model: '',
      expr: '',
    };
  }, [editing]);

  // Values currently in the open dialog (falls back to what it was opened with).
  const formValues = liveValues || initValues;

  // Models available to pick as the rule's target model. Model routing is scoped
  // to the chosen channels, so this is the INTERSECTION of what every selected
  // channel can serve — picking something only some of them offer would leave the
  // others unable to run it, and the engine no longer filters them out (channel
  // routing takes precedence), so the request would fail at the upstream instead.
  //
  // Mirrors the backend's channelServes(): a channel serves its models list PLUS
  // its model_mapping keys (a mapping key is an external name it answers to).
  // Only ENABLED channels count, matching candidateChannels().
  const targetChannelModels = useMemo(() => {
    const enabled = channels.filter((c) => c.status === 'enabled');
    const ids = formValues.target_channel_ids || [];
    const group = (formValues.target_group || '').trim();
    // Explicit ids take precedence over the group, exactly as the engine does.
    let picked = [];
    if (ids.length) {
      picked = enabled.filter((c) => ids.includes(c.id));
    } else if (group) {
      picked = enabled.filter((c) => c.group === group);
    }

    const served = (c) => {
      const set = new Set(c.models || []);
      Object.keys(c.model_mapping || {}).forEach((m) => set.add(m));
      return set;
    };
    let models = [];
    if (picked.length) {
      const sets = picked.map(served);
      models = Array.from(sets[0]).filter((m) => sets.every((set) => set.has(m))).sort();
    }
    return { count: picked.length, models };
  }, [channels, formValues.target_channel_ids, formValues.target_group]);

  const targetModelValue = String(formValues.target_model ?? '').trim();

  // A saved value can fall outside the current intersection later (a channel was
  // edited, or the rule's targets changed). Keep offering it so opening the
  // dialog never silently drops an existing override, but flag it.
  const targetModelStale =
    targetModelValue !== '' &&
    targetChannelModels.count > 0 &&
    !targetChannelModels.models.includes(targetModelValue);

  const targetModelOptions = useMemo(() => {
    const opts = targetChannelModels.models.map((m) => ({ label: m, value: m }));
    if (targetModelStale) {
      opts.unshift({ label: targetModelValue, value: targetModelValue });
    }
    return opts;
  }, [targetChannelModels.models, targetModelStale, targetModelValue]);

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
      // Trimmed so a whitespace-only entry is sent as "" (= no override) rather
      // than an invisible model name. Sent unconditionally (never undefined) so
      // clearing the field in the editor really removes the override; the
      // row-level enable/disable toggle sends a partial payload without this
      // key, which the API treats as "leave unchanged".
      target_model: String(values.target_model ?? '').trim(),
      expr: (values.expr || '').trim(),
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

  const renderMatch = useCallback((match, record) => {
    const m = match || {};
    const parts = [];
    if (m.groups?.length) parts.push(t('match.groups', { value: m.groups.join(', ') }));
    if (m.models?.length) parts.push(t('match.models', { value: m.models.join(', ') }));
    if (m.min_tokens != null) parts.push(t('match.min', { value: m.min_tokens }));
    if (m.max_tokens != null) parts.push(t('match.max', { value: m.max_tokens }));
    const expr = record?.expr;
    if (!parts.length && !expr) return <Text type="tertiary">{t('match.any')}</Text>;
    return (
      <Space spacing={4} wrap>
        {parts.map((p) => (
          <Tag key={p} size="small">
            {p}
          </Tag>
        ))}
        {expr ? (
          <Tag color="orange" size="small" style={{ fontFamily: 'var(--semi-font-family-mono, monospace)' }}>
            {expr}
          </Tag>
        ) : null}
      </Space>
    );
  }, [t]);

  // Target summary: which channels/group serve the rule, plus the model
  // override when the rule sets one, so operators can see at a glance which
  // rules change the model. target_model is omitempty server-side, hence the
  // undefined-tolerant read.
  const renderTarget = useCallback(
    (ids, record) => {
      const list = ids || [];
      const overrideModel = String(record.target_model ?? '').trim();
      const override = overrideModel ? (
        <Tooltip content={t('target.modelTooltip')}>
          <Tag
            color="green"
            size="small"
            style={{ fontFamily: 'var(--semi-font-family-mono, monospace)' }}
          >
            {t('target.model', { value: overrideModel })}
          </Tag>
        </Tooltip>
      ) : null;
      let main;
      if (!list.length && record.target_group) {
        main = <Tag color="violet" size="small">{t('target.group', { value: record.target_group })}</Tag>;
      } else if (!list.length) {
        main = <Text type="tertiary">{t('target.none')}</Text>;
      } else {
        main = list.map((id) => (
          <Tag key={id} color="blue" size="small">
            {channelName(id)}
          </Tag>
        ));
      }
      return (
        <Space spacing={4} wrap>
          {main}
          {override}
        </Space>
      );
    },
    [channelName, t]
  );

  const columns = useMemo(
    () => [
      { title: t('columns.priority'), dataIndex: 'priority', width: 90, sorter: byPriority, defaultSortOrder: 'ascend' },
      { title: t('columns.name'), dataIndex: 'name', width: 160 },
      { title: t('columns.match'), dataIndex: 'match', render: renderMatch },
      {
        title: t('columns.targetChannels'),
        dataIndex: 'target_channel_ids',
        render: renderTarget,
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
    [renderMatch, renderTarget, handleToggle, openEdit, handleDelete, t]
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

      {/* Routing classifier (small-model probe) settings. Collapsed by default
          so it doesn't crowd the rule list. */}
      <Collapse style={{ marginBottom: 16 }}>
        <Collapse.Panel header={t('probe.title')} itemKey="probe">
          <Card bodyStyle={{ padding: 16 }}>
            <Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 12, whiteSpace: 'pre-line' }}>
              {t('probe.help')}
            </Text>
            <Space align="center" style={{ marginBottom: 12 }}>
              <Switch checked={probe.mock} onChange={(v) => setProbe((p) => ({ ...p, mock: v }))} />
              <Text>{probe.mock ? t('probe.modeMock') : t('probe.modeReal')}</Text>
            </Space>
            <div style={{ maxWidth: 560 }}>
              <Text size="small" type="tertiary">{t('probe.url')}</Text>
              <Input
                value={probe.url}
                onChange={(v) => setProbe((p) => ({ ...p, url: v }))}
                placeholder={t('probe.urlPlaceholder')}
                disabled={probe.mock}
                style={{ marginTop: 4, marginBottom: 12 }}
              />
              <Text size="small" type="tertiary">{t('probe.region')}</Text>
              <Input
                value={probe.region}
                onChange={(v) => setProbe((p) => ({ ...p, region: v }))}
                placeholder="us-east-1"
                disabled={probe.mock}
                style={{ marginTop: 4, marginBottom: 12, maxWidth: 220 }}
              />
              <Space>
                <Button theme="solid" type="primary" loading={probeSaving} onClick={saveProbe}>
                  {t('probe.save')}
                </Button>
                <Button
                  loading={probeTesting}
                  disabled={!probe.url}
                  onClick={testProbe}
                >
                  {t('probe.test')}
                </Button>
              </Space>
              {probeTestResult ? (
                <div style={{ marginTop: 10 }}>
                  {probeTestResult.ok ? (
                    <Tag color="green" type="light">
                      {t('probe.testOk', {
                        latency: probeTestResult.latency_ms ?? 0,
                        w: probeTestResult.result?.w ?? '?',
                        tval: probeTestResult.result?.t ?? '?',
                      })}
                    </Tag>
                  ) : (
                    <Tag color="red" type="light" style={{ maxWidth: 520, whiteSpace: 'normal', height: 'auto' }}>
                      {t('probe.testFail', { error: probeTestResult.error || 'unknown error' })}
                    </Tag>
                  )}
                </div>
              ) : null}
            </div>
          </Card>
        </Collapse.Panel>
      </Collapse>

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
        <Form
          initValues={initValues}
          getFormApi={setFormApi}
          onValueChange={mirrorValues}
        >
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
          <Form.Select
            field="groups"
            label={t('form.groups')}
            placeholder={t('form.groupsPlaceholder')}
            multiple
            filter
            allowCreate
            optionList={groupOptions}
            style={{ width: '100%' }}
          />
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

          <Form.TextArea
            field="expr"
            label={t('form.expr')}
            placeholder={t('form.exprPlaceholder')}
            autosize={{ minRows: 1, maxRows: 3 }}
            style={{ fontFamily: 'var(--semi-font-family-mono, monospace)' }}
          />
          <Text type="tertiary" size="small" style={{ display: 'block', marginTop: -8, marginBottom: 6, whiteSpace: 'pre-line' }}>
            {t('form.exprHelp')}
          </Text>
          <div style={{ marginBottom: 4 }}>
            <Text type="tertiary" size="small" style={{ display: 'block', marginBottom: 4 }}>
              {t('form.exprExamplesTitle')}
            </Text>
            <Space spacing={6} wrap>
              {EXPR_EXAMPLES.map((ex) => (
                <Tooltip key={ex.key} content={t(`form.exprEx.${ex.key}`)}>
                  <Tag
                    color="orange"
                    type="light"
                    style={{ fontFamily: 'var(--semi-font-family-mono, monospace)', cursor: 'pointer' }}
                    onClick={() => formApi && formApi.setValue('expr', ex.expr)}
                  >
                    {ex.expr}
                  </Tag>
                </Tooltip>
              ))}
            </Space>
          </div>

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
          {/* Model routing is SCOPED to the chosen channels: pick the target from
              what those channels actually serve, rather than typing a free-form
              name. Channel routing takes precedence in the engine, so a rule's
              channels are no longer filtered by model name — a typo here would
              therefore not be caught at routing time, it would fail at the
              upstream. A closed list removes that class of misconfiguration.
              Deliberately NO field-level initValue — it would shadow the
              form-level initValues and break edit round-tripping (ab127e1). */}
          <Form.Select
            field="target_model"
            label={t('form.targetModel')}
            placeholder={
              targetChannelModels.count
                ? t('form.targetModelPlaceholder')
                : t('form.targetModelPickChannelFirst')
            }
            // Without a resolved channel there is nothing to choose from, and
            // with several channels the intersection can legitimately be empty.
            disabled={!targetChannelModels.count || !targetModelOptions.length}
            optionList={targetModelOptions}
            filter
            showClear
            style={{ width: '100%', fontFamily: 'var(--semi-font-family-mono, monospace)' }}
          />
          <Text
            type={targetModelStale ? 'warning' : 'tertiary'}
            size="small"
            style={{ display: 'block', marginTop: -8, marginBottom: 6, whiteSpace: 'pre-line' }}
          >
            {targetModelStale
              ? t('form.targetModelNotServed', { value: targetModelValue })
              : !targetChannelModels.count
                ? t('form.targetModelPickChannelFirst')
                : !targetModelOptions.length
                  ? t('form.targetModelNoCommon')
                  : t('form.targetModelHelp')}
          </Text>
        </Form>
      </Modal>
    </div>
  );
}
