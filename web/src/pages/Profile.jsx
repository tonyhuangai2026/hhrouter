import React from 'react';
import { Card, Descriptions, Typography, Tag, Space } from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../context/AuthContext';
import LanguageSwitcher from '../components/LanguageSwitcher';
import { formatUSD } from '../utils/money';

const { Title, Text } = Typography;

export default function Profile() {
  const { t, i18n } = useTranslation(['profile', 'common']);
  const { user } = useAuth();

  const empty = t('empty');

  // Localize the role into a readable label, falling back to the raw value.
  const rawRole = user?.role || 'user';
  const roleLabel = i18n.exists(`profile:roles.${rawRole}`)
    ? t(`roles.${rawRole}`)
    : rawRole;

  // Localize the account status (enabled/disabled/etc.) via shared common keys,
  // falling back to the raw value when no matching key exists.
  const rawStatus = user?.status;
  const statusLabel = rawStatus
    ? i18n.exists(`common:status.${rawStatus}`)
      ? t(`common:status.${rawStatus}`)
      : rawStatus
    : empty;

  const hasQuota = user?.quota != null;
  const hasUsed = user?.used != null || user?.usedQuota != null;

  const data = [
    { key: t('fields.username'), value: user?.username || empty },
    {
      key: t('fields.displayName'),
      value: user?.displayName || user?.display_name || empty,
    },
    { key: t('fields.email'), value: user?.email || empty },
    {
      key: t('fields.role'),
      value: (
        <Tag color={rawRole === 'admin' ? 'red' : 'blue'}>{roleLabel}</Tag>
      ),
    },
    { key: t('fields.status'), value: statusLabel },
    // Quota / used are micro-USD now → render as USD ($, or ∞ for unlimited).
    ...(hasQuota ? [{ key: t('fields.quota'), value: formatUSD(user.quota) }] : []),
    ...(hasUsed
      ? [{ key: t('fields.used'), value: formatUSD(user.used ?? user.usedQuota) }]
      : []),
  ];

  return (
    <div>
      <Title heading={2} style={{ marginBottom: 24 }}>
        {t('title')}
      </Title>

      <Card
        title={t('sections.account')}
        style={{ maxWidth: 560, marginBottom: 16 }}
      >
        <Descriptions data={data} />
      </Card>

      <Card title={t('sections.preferences')} style={{ maxWidth: 560 }}>
        <Space vertical align="start" spacing="tight">
          <Text strong>{t('language.label')}</Text>
          <Text type="tertiary">{t('language.description')}</Text>
          <LanguageSwitcher style={{ width: 160 }} />
        </Space>
      </Card>
    </div>
  );
}
