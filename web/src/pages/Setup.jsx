import React, { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { Card, Typography, Button, Spin, Banner } from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';
import { getSetupStatus } from '../api/auth';
import { authShellStyle } from './Login';

const { Title, Text, Paragraph } = Typography;

// First screen: calls GET /api/setup/status.
// - No users yet  -> guide first-deployment registration ("the first account becomes admin").
// - Users exist   -> redirect to normal login.
export default function Setup() {
  const { t } = useTranslation('setup');
  const navigate = useNavigate();
  const [loading, setLoading] = useState(true);
  const [needsSetup, setNeedsSetup] = useState(false);
  const [error, setError] = useState(false);

  // Auth pages render outside AppLayout, so set this page's title here.
  useEffect(() => {
    document.title = t('documentTitle');
  }, [t]);

  useEffect(() => {
    let active = true;
    getSetupStatus()
      .then((status) => {
        if (!active) return;
        const hasUsers = status?.hasUsers !== false; // default to "has users" if shape unknown
        if (hasUsers) {
          navigate('/login', { replace: true });
        } else {
          setNeedsSetup(true);
          setLoading(false);
        }
      })
      .catch(() => {
        if (!active) return;
        // Backend unreachable: surface a recoverable state instead of spinning forever.
        setError(true);
        setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [navigate]);

  if (loading) {
    return (
      <div style={authShellStyle}>
        <Spin size="large" tip={t('checkingStatus')} />
      </div>
    );
  }

  return (
    <div style={authShellStyle}>
      <Card style={{ width: 440 }}>
        <Title heading={3}>{t('welcomeTitle')}</Title>
        {error ? (
          <>
            <Banner
              type="warning"
              description={t('error.description')}
              style={{ margin: '12px 0' }}
              closeIcon={null}
            />
            <div style={{ display: 'flex', gap: 12 }}>
              <Button theme="solid" type="primary" onClick={() => navigate('/login')}>
                {t('error.goLogin')}
              </Button>
              <Button onClick={() => navigate('/register')}>{t('error.register')}</Button>
            </div>
          </>
        ) : (
          needsSetup && (
            <>
              <Paragraph type="tertiary" style={{ marginTop: 8 }}>
                {t('firstDeploy.guidance')}
              </Paragraph>
              <Banner
                type="info"
                description={t('firstDeploy.adminNotice')}
                style={{ margin: '12px 0' }}
                closeIcon={null}
              />
              <Button
                theme="solid"
                type="primary"
                block
                onClick={() => navigate('/register')}
              >
                {t('firstDeploy.createButton')}
              </Button>
            </>
          )
        )}
      </Card>
    </div>
  );
}
