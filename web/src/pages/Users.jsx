import React, { useState, useCallback, useMemo, useEffect, useRef } from 'react';
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
  Select,
  RadioGroup,
  Radio,
  SideSheet,
  Descriptions,
  Spin,
  Empty,
} from '@douyinfe/semi-ui';
import { IconPlus, IconRefresh, IconCopy } from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import { mapApiError, errMessage } from '../api/helpers';
import {
  listUsers,
  createUser,
  updateUser,
  deleteUser,
  resetUserPassword,
  userQuotaOp,
} from '../api/users';
import { getSummary } from '../api/dashboard';
import { listLogs } from '../api/logs';
import { listTokens } from '../api/tokens';
import { useAuth } from '../context/AuthContext';
import { formatUSD } from '../utils/money';

const { Title, Text } = Typography;

const PAGE_SIZE = 10;
const SORT_FIELDS = ['created_at', 'used_quota', 'username'];
const STATUS_COLORS = { enabled: 'green', disabled: 'grey', expired: 'red' };

function num(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

function fmtNum(n) {
  return Number(n || 0).toLocaleString();
}

function fmtDate(d) {
  if (!d) return null;
  const date = new Date(d);
  return Number.isNaN(date.getTime()) ? String(d) : date.toLocaleString();
}

// Derive usage stats from a dashboard summary payload. Mirrors the field
// fallbacks the Dashboard page uses so we display the same numbers.
function deriveUsage(raw) {
  const s = raw || {};
  const totalRequests = num(s.total_requests ?? s.totalRequests ?? s.requests ?? s.count);
  const successRequests = num(
    s.success_requests ?? s.successRequests ?? s.success ?? s.success_count
  );
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
  const totalTokens = num(s.total_tokens ?? s.totalTokens ?? promptTokens + completionTokens);
  return { totalRequests, successRate, totalTokens };
}

// Prefer the backend's human-readable message for user-management failures so
// the self-protection / last-admin / duplicate 409s surface verbatim; fall
// back to the localized mapping for generic/network errors.
function actionError(e) {
  const status = e?.response?.status;
  const raw = errMessage(e, '');
  if (status && status >= 400 && status < 500 && raw) return raw;
  return mapApiError(e);
}

// Admin-only page. The /users route is already guarded by ProtectedRoute
// adminOnly in App.jsx; we additionally guard the body defensively.
export default function Users() {
  const { t } = useTranslation(['users', 'common']);
  const { user: currentUser, isAdmin } = useAuth();

  // ---- list state (server-side pagination + search + filter + sort) ----
  const [items, setItems] = useState([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(false);

  const [search, setSearch] = useState('');
  const [searchInput, setSearchInput] = useState('');
  const [roleFilter, setRoleFilter] = useState('');
  const [statusFilter, setStatusFilter] = useState('');
  const [sortField, setSortField] = useState('created_at');
  const [sortOrder, setSortOrder] = useState('desc');

  const load = useCallback(
    async (targetPage = 1) => {
      setLoading(true);
      try {
        const params = {
          page: targetPage,
          page_size: PAGE_SIZE,
          sort: sortField,
          order: sortOrder,
        };
        if (search) params.search = search;
        if (roleFilter) params.role = roleFilter;
        if (statusFilter) params.status = statusFilter;
        const res = await listUsers(params);
        setItems(res.items);
        setTotal(res.total);
        setPage(targetPage);
      } catch (e) {
        Toast.error(mapApiError(e));
      } finally {
        setLoading(false);
      }
    },
    [search, roleFilter, statusFilter, sortField, sortOrder]
  );

  // Reload from page 1 whenever any filter/sort/search changes.
  const filtersKey = `${search}|${roleFilter}|${statusFilter}|${sortField}|${sortOrder}`;
  useEffect(() => {
    if (!isAdmin) return;
    load(1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filtersKey, isAdmin]);

  // Debounce the search box -> backend search param.
  const debounceRef = useRef(null);
  const onSearchChange = useCallback((v) => {
    setSearchInput(v);
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => setSearch(v.trim()), 400);
  }, []);
  useEffect(() => () => debounceRef.current && clearTimeout(debounceRef.current), []);

  // ---- create / edit modal ----
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState(null); // null => create mode
  const [submitting, setSubmitting] = useState(false);
  const [formApi, setFormApi] = useState(null);

  // ---- reset-password modal + one-time reveal ----
  const [pwTarget, setPwTarget] = useState(null);
  const [pwSubmitting, setPwSubmitting] = useState(false);
  const [pwFormApi, setPwFormApi] = useState(null);
  const [revealedPassword, setRevealedPassword] = useState(null);

  // ---- quota modal ----
  const [quotaTarget, setQuotaTarget] = useState(null);
  const [quotaSubmitting, setQuotaSubmitting] = useState(false);
  const [quotaFormApi, setQuotaFormApi] = useState(null);
  const [quotaOp, setQuotaOp] = useState('add');

  // ---- detail drawer ----
  const [detailTarget, setDetailTarget] = useState(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailUsage, setDetailUsage] = useState(null);
  const [detailTokens, setDetailTokens] = useState([]);

  const roleOptions = useMemo(
    () => [
      { label: t('role.admin'), value: 'admin' },
      { label: t('role.user'), value: 'user' },
    ],
    [t]
  );

  const statusOptions = useMemo(
    () => [
      { label: t('common:status.enabled'), value: 'enabled' },
      { label: t('common:status.disabled'), value: 'disabled' },
    ],
    [t]
  );

  const roleFilterOptions = useMemo(
    () => [{ label: t('toolbar.roleFilter'), value: '' }, ...roleOptions],
    [t, roleOptions]
  );

  const statusFilterOptions = useMemo(
    () => [{ label: t('toolbar.statusFilter'), value: '' }, ...statusOptions],
    [t, statusOptions]
  );

  const sortFieldOptions = useMemo(
    () => [
      { label: t('sort.createdAt'), value: 'created_at' },
      { label: t('sort.usedQuota'), value: 'used_quota' },
      { label: t('sort.username'), value: 'username' },
    ],
    [t]
  );

  const sortOrderOptions = useMemo(
    () => [
      { label: t('sort.desc'), value: 'desc' },
      { label: t('sort.asc'), value: 'asc' },
    ],
    [t]
  );

  const emailRule = useMemo(
    () => ({ type: 'email', message: t('create.emailInvalid') }),
    [t]
  );

  // -------- create / edit --------
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
        role: editing.role || 'user',
        status: editing.status || 'enabled',
        quota: editing.quota ?? 0,
        group: editing.group || 'default',
        display_name: editing.display_name || editing.displayName || '',
        email: editing.email || '',
      };
    }
    return {
      username: '',
      password: '',
      display_name: '',
      email: '',
      role: 'user',
      status: 'enabled',
      quota: 0,
      group: 'default',
    };
  }, [editing]);

  const editingSelf = editing && currentUser && editing.id === currentUser.id;

  const handleSubmit = useCallback(async () => {
    if (!formApi) return;
    let values;
    try {
      values = await formApi.validate();
    } catch {
      return;
    }
    setSubmitting(true);
    try {
      if (editing) {
        const payload = {
          quota: Number(values.quota),
          group: values.group || 'default',
          display_name: values.display_name || '',
          email: values.email || '',
        };
        // Self-protection mirror: never send role/status changes for own row.
        if (!editingSelf) {
          payload.role = values.role;
          payload.status = values.status;
        }
        await updateUser(editing.id, payload);
        Toast.success(t('toast.updated'));
        setModalOpen(false);
        load(page);
      } else {
        await createUser({
          username: values.username,
          password: values.password || '',
          display_name: values.display_name || '',
          email: values.email || '',
          role: values.role,
          status: values.status,
          quota: Number(values.quota),
          group: values.group || 'default',
        });
        Toast.success(t('toast.created'));
        setModalOpen(false);
        load(1);
      }
    } catch (e) {
      Toast.error(actionError(e));
    } finally {
      setSubmitting(false);
    }
  }, [formApi, editing, editingSelf, load, page, t]);

  // -------- reset password --------
  const openResetPassword = useCallback((record) => {
    setPwTarget(record);
  }, []);

  const handleResetPassword = useCallback(async () => {
    if (!pwTarget) return;
    let values = {};
    if (pwFormApi) {
      try {
        values = await pwFormApi.validate();
      } catch {
        values = {};
      }
    }
    setPwSubmitting(true);
    try {
      const res = await resetUserPassword(pwTarget.id, { password: values.password || '' });
      const plaintext = res?.password || res?.plaintext || null;
      setPwTarget(null);
      if (plaintext) {
        setRevealedPassword(plaintext);
      } else {
        Toast.warning(t('resetPassword.noPassword'));
      }
      Toast.success(t('toast.passwordReset'));
    } catch (e) {
      Toast.error(actionError(e));
    } finally {
      setPwSubmitting(false);
    }
  }, [pwTarget, pwFormApi, t]);

  // -------- quota --------
  const openQuota = useCallback((record) => {
    setQuotaOp('add');
    setQuotaTarget(record);
  }, []);

  const handleQuota = useCallback(async () => {
    if (!quotaTarget) return;
    let amount;
    if (quotaOp !== 'reset_used') {
      let values;
      try {
        values = await quotaFormApi?.validate();
      } catch {
        return;
      }
      amount = Number(values?.amount);
      if (!Number.isFinite(amount)) {
        Toast.error(t('quotaOp.amountRequired'));
        return;
      }
    }
    setQuotaSubmitting(true);
    try {
      await userQuotaOp(quotaTarget.id, { op: quotaOp, amount });
      Toast.success(t('toast.quotaUpdated'));
      setQuotaTarget(null);
      load(page);
    } catch (e) {
      Toast.error(actionError(e));
    } finally {
      setQuotaSubmitting(false);
    }
  }, [quotaTarget, quotaOp, quotaFormApi, load, page, t]);

  // -------- delete --------
  const handleDelete = useCallback(
    async (record) => {
      try {
        await deleteUser(record.id);
        Toast.success(t('toast.deleted'));
        // If we just removed the last row on the page, step back a page.
        const nextPage = items.length === 1 && page > 1 ? page - 1 : page;
        load(nextPage);
      } catch (e) {
        Toast.error(actionError(e));
      }
    },
    [items.length, load, page, t]
  );

  // -------- enable / disable --------
  const handleToggleStatus = useCallback(
    async (record) => {
      const next = record.status === 'enabled' ? 'disabled' : 'enabled';
      try {
        await updateUser(record.id, { status: next });
        Toast.success(next === 'enabled' ? t('toast.enabled') : t('toast.disabled'));
        load(page);
      } catch (e) {
        Toast.error(actionError(e));
      }
    },
    [load, page, t]
  );

  // -------- detail drawer --------
  const openDetail = useCallback(
    async (record) => {
      setDetailTarget(record);
      setDetailLoading(true);
      setDetailUsage(null);
      setDetailTokens([]);
      try {
        // Summary -> usage headline; tokens -> token list. The logs query
        // (?user_id) doubles as a request-count fallback when the summary
        // payload doesn't carry a request total.
        const [summary, tokensRes, logsRes] = await Promise.all([
          getSummary({ user_id: record.id }),
          listTokens({ user_id: record.id, page: 1, page_size: 100 }),
          listLogs({ user_id: record.id, page: 1, page_size: 1 }),
        ]);
        const usage = deriveUsage(summary);
        if (!usage.totalRequests && logsRes.total) usage.totalRequests = logsRes.total;
        setDetailUsage(usage);
        setDetailTokens(tokensRes.items || []);
      } catch (e) {
        Toast.error(actionError(e) || t('detail.loadFailed'));
      } finally {
        setDetailLoading(false);
      }
    },
    [t]
  );

  const copyText = useCallback(
    async (value) => {
      try {
        if (navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(value);
          Toast.success(t('toast.copied'));
          return;
        }
        throw new Error('clipboard unavailable');
      } catch {
        Toast.warning(t('toast.copyManual'));
      }
    },
    [t]
  );

  const renderQuotaCell = useCallback(
    (q, r) => {
      if (q === -1 || q == null) return <Tag color="blue">{t('common:labels.unlimited')}</Tag>;
      // Quota / used_quota are micro-USD now → render as USD (used / total).
      return (
        <Text>
          {formatUSD(r.used_quota ?? 0)} / {formatUSD(q)}
        </Text>
      );
    },
    [t]
  );

  const columns = useMemo(
    () => [
      { title: t('columns.username'), dataIndex: 'username', width: 150 },
      {
        title: t('columns.displayName'),
        width: 140,
        render: (_, r) => r.display_name || r.displayName || <Text type="tertiary">{t('display.none')}</Text>,
      },
      {
        title: t('columns.role'),
        dataIndex: 'role',
        width: 90,
        render: (role) => (
          <Tag color={role === 'admin' ? 'red' : 'blue'}>
            {role === 'admin' ? t('role.admin') : t('role.user')}
          </Tag>
        ),
      },
      {
        title: t('columns.status'),
        dataIndex: 'status',
        width: 90,
        render: (s) => (
          <Tag color={s === 'enabled' ? 'green' : 'grey'}>
            {s === 'enabled' ? t('common:status.enabled') : t('common:status.disabled')}
          </Tag>
        ),
      },
      {
        title: t('columns.group'),
        dataIndex: 'group',
        width: 100,
        render: (g) => g || 'default',
      },
      {
        title: t('columns.quota'),
        dataIndex: 'quota',
        width: 150,
        render: renderQuotaCell,
      },
      {
        title: t('columns.lastLogin'),
        dataIndex: 'last_login_at',
        width: 160,
        render: (v, r) => {
          const d = fmtDate(v ?? r.lastLoginAt);
          return d || <Text type="tertiary">{t('display.never')}</Text>;
        },
      },
      {
        title: t('columns.createdAt'),
        dataIndex: 'created_at',
        width: 160,
        render: (v, r) => fmtDate(v ?? r.createdAt) || <Text type="tertiary">{t('display.none')}</Text>,
      },
      {
        title: t('common:labels.actions'),
        width: 360,
        fixed: 'right',
        render: (_, record) => {
          const isSelf = currentUser && record.id === currentUser.id;
          return (
            <Space spacing={4} wrap>
              <Button size="small" theme="borderless" onClick={() => openDetail(record)}>
                {t('actions.view')}
              </Button>
              <Button size="small" theme="borderless" onClick={() => openEdit(record)}>
                {t('actions.edit')}
              </Button>
              <Button size="small" theme="borderless" onClick={() => openQuota(record)}>
                {t('actions.quota')}
              </Button>
              <Button size="small" theme="borderless" onClick={() => openResetPassword(record)}>
                {t('actions.resetPassword')}
              </Button>
              {/* Self-protection mirror: hide disable + delete for own row. */}
              {!isSelf && (
                <Button size="small" theme="borderless" onClick={() => handleToggleStatus(record)}>
                  {record.status === 'enabled' ? t('actions.disable') : t('actions.enable')}
                </Button>
              )}
              {!isSelf && (
                <Popconfirm
                  title={t('confirm.deleteTitle')}
                  content={t('confirm.deleteContent', { name: record.username })}
                  okType="danger"
                  okText={t('common:actions.delete')}
                  cancelText={t('common:actions.cancel')}
                  onConfirm={() => handleDelete(record)}
                >
                  <Button size="small" theme="borderless" type="danger">
                    {t('actions.delete')}
                  </Button>
                </Popconfirm>
              )}
            </Space>
          );
        },
      },
    ],
    [
      t,
      currentUser,
      renderQuotaCell,
      openDetail,
      openEdit,
      openQuota,
      openResetPassword,
      handleToggleStatus,
      handleDelete,
    ]
  );

  if (!isAdmin) {
    return (
      <div>
        <Title heading={2}>{t('title')}</Title>
        <Text type="danger">{t('adminRequired')}</Text>
      </div>
    );
  }

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

      {/* Toolbar: search + filters + sort */}
      <Space wrap align="center" style={{ marginBottom: 16 }}>
        <Input
          prefix={<span />}
          placeholder={t('toolbar.searchPlaceholder')}
          value={searchInput}
          onChange={onSearchChange}
          showClear
          style={{ width: 240 }}
        />
        <Select
          value={roleFilter}
          onChange={setRoleFilter}
          optionList={roleFilterOptions}
          style={{ width: 140 }}
          aria-label={t('columns.role')}
        />
        <Select
          value={statusFilter}
          onChange={setStatusFilter}
          optionList={statusFilterOptions}
          style={{ width: 140 }}
          aria-label={t('columns.status')}
        />
        <Select
          prefix={t('toolbar.sortField')}
          value={sortField}
          onChange={setSortField}
          optionList={sortFieldOptions}
          style={{ width: 180 }}
        />
        <Select
          value={sortOrder}
          onChange={setSortOrder}
          optionList={sortOrderOptions}
          style={{ width: 130 }}
          aria-label={t('toolbar.sortOrder')}
        />
      </Space>

      <Table
        columns={columns}
        dataSource={items}
        loading={loading}
        rowKey="id"
        empty={t('common:state.noData')}
        scroll={{ x: 'max-content' }}
        pagination={{
          currentPage: page,
          pageSize: PAGE_SIZE,
          total,
          onPageChange: (p) => load(p),
        }}
      />

      {/* Create / edit modal */}
      <Modal
        title={
          editing ? t('edit.titleWithName', { name: editing.username }) : t('create.title')
        }
        visible={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleSubmit}
        confirmLoading={submitting}
        okText={editing ? t('common:actions.save') : t('common:actions.create')}
        cancelText={t('common:actions.cancel')}
        maskClosable={false}
        width={480}
      >
        <Form key={editing ? `edit-${editing.id}` : 'create'} initValues={initValues} getFormApi={setFormApi}>
          {!editing && (
            <>
              <Form.Input
                field="username"
                label={t('create.usernameLabel')}
                rules={[{ required: true, message: t('create.usernameRequired') }]}
              />
              <Form.Input
                field="password"
                label={t('create.passwordLabel')}
                mode="password"
                placeholder={t('create.passwordPlaceholder')}
                extraText={t('create.passwordHint')}
              />
            </>
          )}
          <Form.Input field="display_name" label={t('create.displayNameLabel')} />
          <Form.Input field="email" label={t('create.emailLabel')} rules={[emailRule]} />
          <Form.Select
            field="role"
            label={t('edit.roleLabel')}
            optionList={roleOptions}
            style={{ width: '100%' }}
            disabled={editingSelf}
            extraText={editingSelf ? t('edit.selfRoleHint') : undefined}
          />
          <Form.Select
            field="status"
            label={t('edit.statusLabel')}
            optionList={statusOptions}
            style={{ width: '100%' }}
            disabled={editingSelf}
            extraText={editingSelf ? t('edit.selfStatusHint') : undefined}
          />
          <Form.InputNumber
            field="quota"
            label={t('edit.quotaLabel')}
            min={-1}
            style={{ width: '100%' }}
          />
          <Form.Input
            field="group"
            label={t('edit.groupLabel')}
            placeholder={t('edit.groupPlaceholder')}
          />
        </Form>
      </Modal>

      {/* Reset-password modal */}
      <Modal
        title={pwTarget ? t('resetPassword.titleWithName', { name: pwTarget.username }) : t('resetPassword.title')}
        visible={!!pwTarget}
        onCancel={() => setPwTarget(null)}
        onOk={handleResetPassword}
        confirmLoading={pwSubmitting}
        okText={t('resetPassword.confirm')}
        cancelText={t('common:actions.cancel')}
        maskClosable={false}
        width={460}
      >
        <Form key={pwTarget ? `pw-${pwTarget.id}` : 'pw'} initValues={{ password: '' }} getFormApi={setPwFormApi}>
          <Form.Input
            field="password"
            label={t('resetPassword.passwordLabel')}
            mode="password"
            placeholder={t('resetPassword.passwordPlaceholder')}
            extraText={t('resetPassword.passwordHint')}
          />
        </Form>
      </Modal>

      {/* One-time temporary-password reveal */}
      <Modal
        title={t('resetPassword.resultTitle')}
        visible={!!revealedPassword}
        onCancel={() => setRevealedPassword(null)}
        footer={
          <Button theme="solid" type="primary" onClick={() => setRevealedPassword(null)}>
            {t('common:actions.close')}
          </Button>
        }
        maskClosable={false}
        width={480}
      >
        <Banner type="warning" description={t('resetPassword.resultWarning')} style={{ marginBottom: 16 }} />
        <Space style={{ width: '100%' }}>
          <Input value={revealedPassword || ''} readonly style={{ flex: 1, fontFamily: 'monospace' }} />
          <Button icon={<IconCopy />} onClick={() => copyText(revealedPassword)}>
            {t('common:actions.copy')}
          </Button>
        </Space>
      </Modal>

      {/* Quota modal */}
      <Modal
        title={quotaTarget ? t('quotaOp.titleWithName', { name: quotaTarget.username }) : t('quotaOp.title')}
        visible={!!quotaTarget}
        onCancel={() => setQuotaTarget(null)}
        onOk={handleQuota}
        confirmLoading={quotaSubmitting}
        okText={t('quotaOp.confirm')}
        cancelText={t('common:actions.cancel')}
        maskClosable={false}
        width={460}
      >
        {quotaTarget && (
          <div style={{ marginBottom: 12 }}>
            <Text type="tertiary">{t('quotaOp.currentLabel')}: </Text>
            <Text>
              {quotaTarget.quota === -1 || quotaTarget.quota == null
                ? t('common:labels.unlimited')
                : `${fmtNum(quotaTarget.used_quota ?? 0)} / ${fmtNum(quotaTarget.quota)}`}
            </Text>
          </div>
        )}
        <div style={{ marginBottom: 12 }}>
          <Text>{t('quotaOp.opLabel')}</Text>
          <RadioGroup
            type="button"
            value={quotaOp}
            onChange={(e) => setQuotaOp(e.target.value)}
            style={{ marginTop: 8 }}
          >
            <Radio value="add">{t('quotaOp.add')}</Radio>
            <Radio value="set">{t('quotaOp.set')}</Radio>
            <Radio value="reset_used">{t('quotaOp.resetUsed')}</Radio>
          </RadioGroup>
        </div>
        {quotaOp === 'reset_used' ? (
          <Text type="tertiary">{t('quotaOp.resetUsedHint')}</Text>
        ) : (
          <Form key={quotaOp} initValues={{ amount: quotaOp === 'set' ? 0 : 1 }} getFormApi={setQuotaFormApi}>
            <Form.InputNumber
              field="amount"
              label={t('quotaOp.amountLabel')}
              min={quotaOp === 'set' ? -1 : 1}
              style={{ width: '100%' }}
              rules={[{ required: true, message: t('quotaOp.amountRequired') }]}
              extraText={quotaOp === 'add' ? t('quotaOp.addHint') : t('quotaOp.setHint')}
            />
          </Form>
        )}
      </Modal>

      {/* Detail drawer */}
      <SideSheet
        title={detailTarget ? t('detail.titleWithName', { name: detailTarget.username }) : t('detail.title')}
        visible={!!detailTarget}
        onCancel={() => setDetailTarget(null)}
        width={520}
      >
        <Spin spinning={detailLoading}>
          {detailTarget && (
            <div>
              <Title heading={6} style={{ marginTop: 0 }}>
                {t('detail.profileSection')}
              </Title>
              <Descriptions
                row
                size="small"
                data={[
                  { key: t('columns.username'), value: detailTarget.username },
                  {
                    key: t('columns.displayName'),
                    value: detailTarget.display_name || detailTarget.displayName || '-',
                  },
                  { key: t('columns.email'), value: detailTarget.email || '-' },
                  {
                    key: t('columns.role'),
                    value: detailTarget.role === 'admin' ? t('role.admin') : t('role.user'),
                  },
                  {
                    key: t('columns.status'),
                    value:
                      detailTarget.status === 'enabled'
                        ? t('common:status.enabled')
                        : t('common:status.disabled'),
                  },
                  { key: t('columns.group'), value: detailTarget.group || 'default' },
                  {
                    key: t('columns.lastLogin'),
                    value: fmtDate(detailTarget.last_login_at ?? detailTarget.lastLoginAt) || t('display.never'),
                  },
                ]}
              />

              <Title heading={6}>{t('detail.usageSection')}</Title>
              <Descriptions
                row
                size="small"
                data={[
                  { key: t('detail.totalRequests'), value: fmtNum(detailUsage?.totalRequests) },
                  { key: t('detail.totalTokens'), value: fmtNum(detailUsage?.totalTokens) },
                  {
                    key: t('detail.successRate'),
                    value: `${(detailUsage?.successRate ?? 0).toFixed(1)}%`,
                  },
                ]}
              />

              <Title heading={6}>{t('detail.tokensSection')}</Title>
              {detailTokens.length === 0 && !detailLoading ? (
                <Empty description={t('detail.noTokens')} style={{ padding: 24 }} />
              ) : (
                <Table
                  size="small"
                  rowKey="id"
                  pagination={false}
                  dataSource={detailTokens}
                  columns={[
                    { title: t('detail.tokenName'), dataIndex: 'name', width: 140 },
                    {
                      title: t('detail.tokenStatus'),
                      dataIndex: 'status',
                      width: 90,
                      render: (s) => <Tag color={STATUS_COLORS[s] || 'grey'}>{s}</Tag>,
                    },
                    {
                      title: t('detail.tokenGroup'),
                      dataIndex: 'group',
                      width: 90,
                      render: (g) => g || 'default',
                    },
                    {
                      title: t('detail.tokenQuota'),
                      dataIndex: 'quota',
                      render: (q, r) =>
                        q === -1 || q == null
                          ? t('common:labels.unlimited')
                          : `${fmtNum(r.used_quota ?? 0)} / ${fmtNum(q)}`,
                    },
                  ]}
                />
              )}
            </div>
          )}
        </Spin>
      </SideSheet>
    </div>
  );
}
