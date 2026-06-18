import React, { useMemo, useEffect } from 'react';
import { Outlet, useNavigate, useLocation } from 'react-router-dom';
import {
  Layout,
  Nav,
  Button,
  Avatar,
  Dropdown,
  Typography,
  Tooltip,
} from '@douyinfe/semi-ui';
import {
  IconHome,
  IconServer,
  IconBranch,
  IconKey,
  IconArticle,
  IconComment,
  IconUserGroup,
  IconUser,
  IconCreditCard,
  IconMoon,
  IconSun,
  IconExit,
} from '@douyinfe/semi-icons';
import { useTranslation } from 'react-i18next';
import { useAuth } from '../context/AuthContext';
import { useTheme } from '../context/ThemeContext';
import LanguageSwitcher from './LanguageSwitcher';

const { Header, Sider, Content } = Layout;
const { Text } = Typography;

// Menu definition. `text` is an i18n key under the `layout` namespace (nav.*).
// `admin` items are hidden for non-admin users (AC: admin-only by role).
const MENU_ITEMS = [
  { itemKey: '/dashboard', textKey: 'nav.dashboard', icon: <IconHome /> },
  { itemKey: '/channels', textKey: 'nav.channels', icon: <IconServer /> },
  { itemKey: '/rules', textKey: 'nav.rules', icon: <IconBranch /> },
  { itemKey: '/tokens', textKey: 'nav.tokens', icon: <IconKey /> },
  { itemKey: '/logs', textKey: 'nav.logs', icon: <IconArticle /> },
  { itemKey: '/playground', textKey: 'nav.playground', icon: <IconComment />, admin: true },
  { itemKey: '/pricing', textKey: 'nav.pricing', icon: <IconCreditCard />, admin: true },
  { itemKey: '/users', textKey: 'nav.users', icon: <IconUserGroup />, admin: true },
  { itemKey: '/profile', textKey: 'nav.profile', icon: <IconUser /> },
];

export default function AppLayout() {
  const navigate = useNavigate();
  const location = useLocation();
  const { t, i18n } = useTranslation('layout');
  const { user, isAdmin, logout } = useAuth();
  const { isDark, toggleTheme } = useTheme();

  const items = useMemo(
    () =>
      MENU_ITEMS.filter((item) => !item.admin || isAdmin).map((item) => ({
        itemKey: item.itemKey,
        icon: item.icon,
        text: t(item.textKey),
      })),
    [isAdmin, t, i18n.language]
  );

  // Keep document.title in sync with the system name + current page, and refresh
  // it whenever the route or active language changes.
  useEffect(() => {
    const matched = MENU_ITEMS.find((item) => item.itemKey === location.pathname);
    const systemName = t('systemName');
    document.title = matched
      ? `${t(matched.textKey)} - ${systemName}`
      : systemName;
  }, [location.pathname, i18n.language, t]);

  const handleLogout = () => {
    logout();
    navigate('/login', { replace: true });
  };

  const displayName =
    user?.displayName || user?.display_name || user?.username || t('user.fallbackName');

  return (
    <Layout style={{ height: '100vh' }}>
      <Sider style={{ backgroundColor: 'var(--semi-color-bg-1)' }}>
        <Nav
          style={{ height: '100%' }}
          selectedKeys={[location.pathname]}
          items={items}
          onSelect={({ itemKey }) => navigate(itemKey)}
          header={{
            logo: <IconServer style={{ fontSize: 36 }} />,
            text: t('appName'),
          }}
          footer={{ collapseButton: true }}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'flex-end',
            gap: 12,
            padding: '0 24px',
            backgroundColor: 'var(--semi-color-bg-1)',
            borderBottom: '1px solid var(--semi-color-border)',
          }}
        >
          <Tooltip content={isDark ? t('theme.toLight') : t('theme.toDark')}>
            <Button
              theme="borderless"
              icon={isDark ? <IconSun /> : <IconMoon />}
              onClick={toggleTheme}
              aria-label="toggle-theme"
            />
          </Tooltip>
          <LanguageSwitcher />
          <Dropdown
            trigger="click"
            position="bottomRight"
            render={
              <Dropdown.Menu>
                <Dropdown.Item onClick={() => navigate('/profile')}>
                  <IconUser style={{ marginRight: 8 }} />
                  {t('userMenu.profile')}
                </Dropdown.Item>
                <Dropdown.Divider />
                <Dropdown.Item type="danger" onClick={handleLogout}>
                  <IconExit style={{ marginRight: 8 }} />
                  {t('userMenu.logout')}
                </Dropdown.Item>
              </Dropdown.Menu>
            }
          >
            <div style={{ display: 'flex', alignItems: 'center', cursor: 'pointer', gap: 8 }}>
              <Avatar size="small" color="blue">
                {displayName.slice(0, 1).toUpperCase()}
              </Avatar>
              <Text>{displayName}</Text>
            </div>
          </Dropdown>
        </Header>
        <Content
          style={{
            padding: 24,
            overflow: 'auto',
            backgroundColor: 'var(--semi-color-bg-0)',
          }}
        >
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
