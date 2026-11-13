import { useState, useEffect } from 'react';
import { type PageId } from './nav';
import { Sidebar } from './components/Sidebar';
import { TopBar } from './components/TopBar';
import { MobileNav } from './components/MobileNav';
import { HomePage } from './pages/HomePage';
import { PlaceholderPage } from './pages/PlaceholderPage';
import { SettingsPage } from './pages/SettingsPage';
import { AgentsPage } from './pages/AgentsPage';
import { ActivityPage } from './pages/ActivityPage';
import { SetupWizard } from './pages/SetupWizard';
import { apiMe, apiLogout, apiGetSetupState, apiLogin } from './api';
import { initLangFromCache, setLangFromSystemSettings, onLangChange, t } from './i18n';

type AppState = 'loading' | 'setup' | 'wizard' | 'login' | 'app';

export default function App() {
  const [appState, setAppState] = useState<AppState>('loading');
  const [page, setPage] = useState<PageId>('home');
  const [_username, setUsername] = useState('');

  // Initialize language from cache (localStorage) immediately
  useEffect(() => {
    initLangFromCache();
  }, []);

  // Subscribe to language changes to trigger re-render
  useEffect(() => {
    const unsubscribe = onLangChange(() => {
      // Force re-render by updating a dummy state
      setAppState((s) => s);
    });
    return unsubscribe;
  }, []);

  // Check setup state on mount
  useEffect(() => {
    let mounted = true;
    apiGetSetupState()
      .then((state) => {
        if (!mounted) return;
        if (state.setup_complete) {
          // Setup complete - check for token
          const token = localStorage.getItem('kelvios_token');
          if (token) {
            // Has token - verify it works
            apiMe()
              .then((me) => {
                if (mounted) {
                  setUsername(me.username);
                  setLangFromSystemSettings('en'); // Will be updated by SettingsPage load
                  setAppState('app');
                }
              })
              .catch(() => {
                if (mounted) {
                  localStorage.removeItem('kelvios_token');
                  setAppState('login');
                }
              });
          } else {
            // No token - show login page
            setAppState('login');
          }
        } else {
          // Setup incomplete - show wizard at current stage
          setAppState('wizard');
        }
      })
      .catch(() => {
        if (mounted) {
          // On error, assume setup needed
          setAppState('wizard');
        }
      });
    return () => { mounted = false; };
  }, []);

  const handleWizardComplete = () => {
    setAppState('app');
  };

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    const formData = new FormData(e.currentTarget as HTMLFormElement);
    const user = formData.get('username') as string;
    const pass = formData.get('password') as string;

    try {
      const loginRes = await apiLogin({ username: user.trim(), password: pass });
      localStorage.setItem('kelvios_token', loginRes.token);
      setUsername(user.trim());
      setAppState('app');
    } catch (err) {
      alert(err instanceof Error ? err.message : 'Login failed');
    }
  };

  const handleLogout = async () => {
    await apiLogout();
    localStorage.removeItem('kelvios_token');
    setAppState('login');
    setPage('home');
    setUsername('');
  };

  if (appState === 'loading') {
    return <div className="app-loading">{t('app.loading')}</div>;
  }

  if (appState === 'wizard') {
    return <SetupWizard onComplete={handleWizardComplete} />;
  }

  if (appState === 'login') {
    return (
      <div className="setup-page">
        <div className="setup-card">
          <h1 className="setup-title">{t('setup.title')}</h1>
          <p className="setup-subtitle">{t('setup.subtitle')}</p>
          <form onSubmit={handleLogin} className="setup-form">
            <div className="setup-field">
              <label htmlFor="login-username" className="setup-label">
                {t('setup.usernameLabel')}
              </label>
              <input
                id="login-username"
                name="username"
                type="text"
                className="setup-input"
                required
                autoComplete="username"
                autoFocus
              />
            </div>
            <div className="setup-field">
              <label htmlFor="login-password" className="setup-label">
                {t('setup.passwordLabel')}
              </label>
              <input
                id="login-password"
                name="password"
                type="password"
                className="setup-input"
                required
                autoComplete="current-password"
              />
            </div>
            <button type="submit" className="button button--primary setup-submit">
              {t('wizard.step3.submit.login')}
            </button>
          </form>
        </div>
      </div>
    );
  }

  return (
    <div className="app">
      <Sidebar current={page} onNavigate={setPage} />
      <div className="main">
        <TopBar title={t(`page.${page}` as any)} onNavigate={setPage} onLogout={handleLogout} />
        <main className="content">
          {page === 'home' ? (
            <HomePage onNavigate={setPage} />
          ) : page === 'settings' ? (
            <SettingsPage />
          ) : page === 'agents' ? (
            <AgentsPage />
          ) : page === 'activity' ? (
            <ActivityPage />
          ) : (
            <PlaceholderPage page={page} />
          )}
        </main>
      </div>
      <MobileNav current={page} onNavigate={setPage} />
    </div>
  );
}