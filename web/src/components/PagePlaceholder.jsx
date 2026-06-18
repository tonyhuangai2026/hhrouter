import React from 'react';
import { Empty, Typography } from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';

const { Title } = Typography;

// Shared placeholder for pages whose full content arrives in later tasks (T10/T11).
export default function PagePlaceholder({ title, description }) {
  const { t } = useTranslation('layout');
  return (
    <div>
      <Title heading={2} style={{ marginBottom: 24 }}>
        {title}
      </Title>
      <Empty
        title={t('placeholder.title')}
        description={description || t('placeholder.description')}
        style={{ paddingTop: 80 }}
      />
    </div>
  );
}
