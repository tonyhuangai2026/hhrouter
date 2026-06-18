import React, { useState } from 'react';
import { useNavigate, useLocation, Link } from 'react-router-dom';
import { Form, Button, Card, Typography, Toast } from '@douyinfe/semi-ui';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../context/AuthContext';
import { mapApiError } from '../api/helpers';

const { Title, Text } = Typography;

export default function Login() {
  const { t } = useTranslation('login');
  const navigate = useNavigate();
  const location = useLocation();
  const { login } = useAuth();
  const [loading, setLoading] = useState(false);

  const from = location.state?.from?.pathname || '/dashboard';

  const handleSubmit = async (values) => {
    setLoading(true);
    try {
      await login(values);
      Toast.success(t('messages.success'));
      navigate(from, { replace: true });
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
          <Link to="/register">{t('switch.link')}</Link>
        </div>
      </Card>
    </div>
  );
}

export const authShellStyle = {
  minHeight: '100vh',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  backgroundColor: 'var(--semi-color-bg-0)',
};
