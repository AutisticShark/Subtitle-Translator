<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import Admin from './components/Admin.svelte';
  import Auth from './components/Auth.svelte';
  import Brand from './components/Brand.svelte';
  import Dashboard from './components/Dashboard.svelte';
  import History from './components/History.svelte';
  import Security from './components/Security.svelte';
  import Translate from './components/Translate.svelte';
  import { api, ApiError, errorMessage, json } from './lib/api';
  import { readBoot } from './lib/boot';
  import {
    applyTheme,
    appState,
    clearSession,
    navigate,
    notify,
    showView,
    t,
  } from './lib/appState.svelte';
  import type { Catalog, Settings, SetupStatus, Theme, User, View } from './lib/types';
  import { startWorkspace, stopWorkspace } from './lib/workspace.svelte';

  let loading = $state(true);
  let startupError = $state('');
  let themeBusy = $state(false);
  const locales = readBoot().locales;
  const navigation: { view: View; icon: string; label: string; admin?: boolean }[] = [
    { view: 'dashboard', icon: '⌂', label: 'Dashboard' },
    { view: 'translate', icon: '＋', label: 'New translation' },
    { view: 'jobs', icon: '≡', label: 'Translation history' },
    { view: 'security', icon: '◇', label: 'Account security' },
    { view: 'admin', icon: '⚙', label: 'Administration', admin: true },
  ];
  const viewLabels: Record<View, [string, string]> = {
    security: ['Account preferences', 'Account security'],
    dashboard: ['Workspace overview', 'Dashboard'],
    translate: ['Translation workspace', 'New translation'],
    jobs: ['Activity', 'Translation history'],
    admin: ['Panel management', 'Administration'],
  };
  const admin = $derived(appState.user?.role === 'admin');
  const signedIn = $derived(appState.user !== null);

  async function enter(user: User) {
    const [auth, settings] = await Promise.all([
      api<SetupStatus>('/api/auth/setup-status'),
      api<Settings>('/api/settings'),
    ]);
    appState.session++;
    appState.auth = auth;
    appState.settings = settings;
    applyTheme(user.theme);
    appState.user = user;
    showView(window.location.hash.slice(1) || 'dashboard', false);
  }
  async function initialize() {
    startupError = '';
    loading = true;
    try {
      // English source strings are better than no sign-in screen at all.
      const catalog = await api<Catalog>('/api/i18n').catch(() => null);
      if (catalog) {
        appState.catalog = catalog;
        document.documentElement.lang = catalog.locale;
      }
      const auth = await api<SetupStatus>('/api/auth/setup-status');
      appState.auth = auth;
      if (auth.configured) {
        try {
          const data = await api<{ user: User }>('/api/auth/me');
          await enter(data.user);
        } catch (cause) {
          if (!(cause instanceof ApiError && cause.status === 401)) throw cause;
          clearSession();
        }
      }
    } catch (cause) {
      startupError = errorMessage(cause);
    } finally {
      loading = false;
    }
  }
  async function expired() {
    clearSession();
    appState.auth = await api<SetupStatus>('/api/auth/setup-status').catch(() => appState.auth);
    notify(t('Your session has ended. Sign in again.'));
  }
  async function logout() {
    stopWorkspace(); // Stop polling before the token is revoked.
    try {
      await api('/api/auth/logout', { method: 'POST' });
    } catch (cause) {
      if (!(cause instanceof ApiError && cause.status === 401)) {
        // The session cookie is still valid; showing the sign-in screen would only
        // hide a live session from the next person at this browser.
        notify(errorMessage(cause));
        startWorkspace(expired);
        return;
      }
    }
    clearSession();
    try {
      appState.auth = await api<SetupStatus>('/api/auth/setup-status');
    } catch (cause) {
      notify(errorMessage(cause));
    }
  }
  async function saveTheme(event: Event) {
    const selector = event.currentTarget as HTMLSelectElement;
    const previous = appState.user?.theme || 'system';
    const generation = appState.session;
    themeBusy = true;
    applyTheme(selector.value as Theme);
    try {
      const data = await json<{ user: User }>('/api/auth/me', { theme: selector.value }, 'PATCH');
      if (generation !== appState.session) return;
      appState.user = data.user;
      applyTheme(data.user.theme);
      notify(t('Theme updated'));
    } catch (cause) {
      if (generation === appState.session) {
        // appState.user.theme did not change, so the bound value would not reset the control.
        selector.value = previous;
        applyTheme(previous);
        notify(errorMessage(cause));
      }
    } finally {
      themeBusy = false;
    }
  }
  function changeLocale(event: Event) {
    const url = new URL(window.location.href);
    url.searchParams.set('lang', (event.currentTarget as HTMLSelectElement).value);
    window.location.assign(url);
  }

  onMount(() => {
    void initialize();
    const hashChanged = () => showView(window.location.hash.slice(1), false);
    window.addEventListener('hashchange', hashChanged);
    const system = window.matchMedia('(prefers-color-scheme: dark)');
    const schemeChanged = () => applyTheme(appState.user?.theme || 'system');
    system.addEventListener('change', schemeChanged);
    return () => {
      appState.session++;
      window.removeEventListener('hashchange', hashChanged);
      system.removeEventListener('change', schemeChanged);
    };
  });
  // Poll only while an account is signed in; stop on sign-out, session change and unmount.
  $effect(() => {
    void appState.session;
    if (!signedIn) return;
    untrack(() => startWorkspace(expired));
    return stopWorkspace;
  });
</script>

<label class="locale-picker">
  <span>{t('Interface language')}</span>
  <select id="localeSelect" aria-label={t('Interface language')} onchange={changeLocale}>
    {#each Object.entries(locales) as [code, label] (code)}
      <option value={code} selected={code === appState.catalog.locale}>{label}</option>
    {/each}
  </select>
</label>

{#if loading}
  <main class="auth-view" aria-busy="true"></main>
{:else if startupError}
  <main class="auth-view">
    <section class="panel auth-panel">
      <Brand />
      <div class="auth-error" role="alert">{startupError}</div>
      <button class="ghost" type="button" onclick={initialize}>{t('Try again')}</button>
    </section>
  </main>
{:else if !appState.user}
  <Auth onSignedIn={enter} />
{:else}
  <div class="app-shell" id="appShell">
    <aside class="sidebar">
      <Brand href="#dashboard" />
      <nav class="primary-nav" aria-label={t('Main navigation')}>
        {#each navigation.filter((item) => !item.admin || admin) as item (item.view)}
          <button
            class="nav-item"
            class:active={appState.view === item.view}
            type="button"
            data-view-button={item.view}
            id={item.view === 'admin' ? 'adminNavButton' : undefined}
            aria-current={appState.view === item.view ? 'page' : 'false'}
            onclick={() => navigate(item.view)}
          >
            <span class="nav-icon" aria-hidden="true">{item.icon}</span>{t(item.label)}
          </button>
        {/each}
      </nav>
      <div class="sidebar-footer">
        <div class="account-chip">
          <span class="account-avatar" id="userInitial" aria-hidden="true"
            >{appState.user.username.slice(0, 1)}</span
          >
          <span
            ><strong id="userName">{appState.user.username}</strong><small id="userRole"
              >{t(admin ? 'Administrator' : 'User')}</small
            ></span
          >
        </div>
        <button class="sign-out" id="logoutButton" type="button" onclick={logout}
          >{t('Sign out')}</button
        >
      </div>
    </aside>

    <div class="app-main">
      <header class="topbar">
        <div>
          <span class="eyebrow" id="viewEyebrow">{t(viewLabels[appState.view][0])}</span>
          <h1 id="viewTitle">{t(viewLabels[appState.view][1])}</h1>
        </div>
        <div class="topbar-actions">
          <label class="theme-picker">
            <span>{t('Appearance')}</span>
            <select
              id="themeSelect"
              aria-label={t('Appearance')}
              value={appState.user.theme}
              disabled={themeBusy}
              onchange={saveTheme}
            >
              <option value="system">{t('System theme')}</option>
              <option value="light">{t('Light')}</option>
              <option value="dark">{t('Dark')}</option>
            </select>
          </label>
          <button
            class="primary topbar-action"
            type="button"
            hidden={appState.view === 'translate'}
            onclick={() => navigate('translate')}>{t('New translation')} <span>→</span></button
          >
        </div>
      </header>

      <main class="content">
        {#key appState.session}
          <section class="app-view" data-view="dashboard" hidden={appState.view !== 'dashboard'}>
            <Dashboard />
          </section>
          <section class="app-view" data-view="translate" hidden={appState.view !== 'translate'}>
            <Translate />
          </section>
          <section class="app-view" data-view="jobs" hidden={appState.view !== 'jobs'}>
            <History />
          </section>
          {#if appState.view === 'security'}
            <section class="app-view" data-view="security">
              <Security />
            </section>
          {/if}
          {#if admin}
            <section
              class="app-view"
              data-view="admin"
              id="adminView"
              hidden={appState.view !== 'admin'}
            >
              <Admin />
            </section>
          {/if}
        {/key}
      </main>
    </div>
  </div>
{/if}
<div class="toast" id="toast" class:show={appState.noticeShown} role="status">
  {appState.notice}
</div>
