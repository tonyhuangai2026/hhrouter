import React, { useState, useEffect } from 'react';
import { useNavigate, Link } from 'react-router-dom';
import { Form, Button, Card, Typography, Toast, Banner } from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../context/AuthContext';
import { getSetupStatus } from '../api/auth';
import { mapApiError } from '../api/helpers';
import { authShellStyle } from './Login';

const { Title, Text } = Typography;

export default function Register() {
  const { t } = useTranslation('register');
  const navigate = useNavigate();
  const { register } = useAuth();
  const [loading, setLoading] = useState(false);
  const [isFirstAccount, setIsFirstAccount] = useState(false);

  // Detect whether this would be the first account (becomes admin).
  useEffect(() => {
    let active = true;
    getSetupStatus()
      .then((status) => {
        if (active) setIsFirstAccount(status?.hasUsers === false);
      })
      .catch(() => {
        /* backend may be unavailable; keep default messaging */
      });
    return () => {
      active = false;
    };
  }, []);

  const handleSubmit = async (values) => {
    if (values.password !== values.confirmPassword) {
      Toast.error(t('validation.passwordMismatch'));
      return;
    }
    setLoading(true);
    try {
      const { confirmPassword, ...payload } = values;
      await register(payload);
      Toast.success(t('messages.success'));
      navigate('/dashboard', { replace: true });
    } catch (err) {
      Toast.error(mapApiError(err));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={authShellStyle}>
      <Card style={{ width: 380 }}>
        <Title heading={3} style={{ marginBottom: 4 }}>
          {t('title')}
        </Title>
        <Text type="tertiary">{t('subtitle')}</Text>
        {isFirstAccount && (
          <Banner
            type="info"
            description={t('firstAccountNotice')}
            style={{ marginTop: 12 }}
            closeIcon={null}
          />
        )}
        <Form onSubmit={handleSubmit} style={{ marginTop: 16 }}>
          <Form.Input
            field="username"
            label={t('fields.username.label')}
            placeholder={t('fields.username.placeholder')}
            rules={[{ required: true, message: t('validation.usernameRequired') }]}
          />
          <Form.Input
            field="password"
            label={t('fields.password.label')}
            mode="password"
            placeholder={t('fields.password.placeholder')}
            rules={[{ required: true, message: t('validation.passwordRequired') }]}
          />
          <Form.Input
            field="confirmPassword"
            label={t('fields.confirmPassword.label')}
            mode="password"
            placeholder={t('fields.confirmPassword.placeholder')}
            rules={[{ required: true, message: t('validation.confirmRequired') }]}
          />
          <Button
            htmlType="submit"
            theme="solid"
            type="primary"
            block
            loading={loading}
            style={{ marginTop: 12 }}
          >
            {t('submit')}
          </Button>
        </Form>
        <div style={{ marginTop: 16, textAlign: 'center' }}>
          <Text type="tertiary">{t('switch.prompt')}</Text>
          <Link to="/login">{t('switch.link')}</Link>
        </div>
      </Card>
    </div>
  );
}
