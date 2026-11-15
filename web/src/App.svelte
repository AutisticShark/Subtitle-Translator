<script lang="ts">
  import { onMount } from 'svelte';
  import Auth from './components/Auth.svelte';
  import Brand from './components/Brand.svelte';
  import Icon from './components/Icon.svelte';
  import Translate from './components/Translate.svelte';
  import JobList from './components/JobList.svelte';
  import SettingsView from './components/Settings.svelte';
  import Users from './components/Users.svelte';
  import Account from './components/Account.svelte';
  import { api, ApiError, errorMessage, json } from './lib/api';
  import { applyTheme, clearSession, navigate, notify, appState, t } from './lib/appState.svelte';
  import type { Catalog, Settings, SetupStatus, Theme, User, View } from './lib/types';

  let loading = $state(true);
  let startupError = $state('');
  let signingOut = $state(false);
  let themeBusy = $state(false);
  function availableLocales(): Record<string, string> {
    try {
      const boot = JSON.parse(document.getElementById('app-boot')?.textContent || '{}') as {
        locales?: { code: string; label: string }[];
      };
      if (Array.isArray(boot.locales) && boot.locales.length) {
        return Object.fromEntries(
          boot.locales
            .filter((locale) => typeof locale.code === 'string' && typeof locale.label === 'string')
            .map((locale) => [locale.code, locale.label]),
        );
      }
    } catch {
      /* Vite serves the shell without server bootstrap data. */
    }
    return { en: 'English', 'zh-TW': '正體中文', 'zh-CN': '简体中文' };
  }
  const locales = availableLocales();
  const navigation: { view: View; label: string; icon: string; admin?: boolean }[] = [
    { view: 'translate', label: 'Translate', icon: 'translate' },
    { view: 'history', label: 'History', icon: 'history' },
    { view: 'settings', label: 'Settings', icon: 'settings', admin: true },
    { view: 'users', label: 'Users', icon: 'users', admin: true },
    { view: 'account', label: 'Account', icon: 'shield' },
  ];
  function syncHash() {
    const requested = window.location.hash.slice(1);
    const match = navigation.find(
      (item) => item.view === requested && (!item.admin || appState.user?.role === 'admin'),
    );
    appState.view = match?.view || 'translate';
  }
  async function signedIn(user: User) {
    const [auth, settings] = await Promise.all([
      api<SetupStatus>('/api/auth/setup-status'),
      api<Settings>('/api/settings'),
    ]);
    appState.session++;
    appState.auth = auth;
    appState.settings = settings;
    applyTheme(user.theme);
    appState.user = user;
    syncHash();
  }
  async function initialize() {
    startupError = '';
    loading = true;
    try {
      const [catalog, auth] = await Promise.all([
        api<Catalog>('/api/i18n'),
        api<SetupStatus>('/api/auth/setup-status'),
      ]);
      appState.catalog = catalog;
      appState.auth = auth;
      document.documentElement.lang = catalog.locale;
      if (auth.configured) {
        try {
          const data = await api<{ user: User }>('/api/auth/me');
          await signedIn(data.user);
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
  async function logout() {
    signingOut = true;
    try {
      try {
        await api('/api/auth/logout', { method: 'POST' });
      } catch (cause) {
        if (!(cause instanceof ApiError && cause.status === 401)) throw cause;
      }
      clearSession();
      window.location.hash = '';
      appState.auth = await api<SetupStatus>('/api/auth/setup-status');
    } catch (cause) {
      notify(errorMessage(cause));
    } finally {
      signingOut = false;
    }
  }
  async function saveTheme(event: Event) {
    const selected = (event.currentTarget as HTMLSelectElement).value as Theme;
    const previous = appState.user?.theme || 'system';
    themeBusy = true;
    const generation = appState.session;
    applyTheme(selected);
    try {
      const data = await json<{ user: User }>('/api/auth/me', { theme: selected }, 'PATCH');
      if (generation !== appState.session) return;
      appState.user = data.user;
      applyTheme(data.user.theme);
    } catch (cause) {
      if (generation === appState.session) {
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
  async function expired() {
    clearSession();
    appState.auth = await api<SetupStatus>('/api/auth/setup-status').catch(() => appState.auth);
    notify(t('Your session has expired. Sign in again.'));
  }
  onMount(() => {
    void initialize();
    window.addEventListener('hashchange', syncHash);
    const system = window.matchMedia('(prefers-color-scheme: dark)');
    const schemeChanged = () => applyTheme(appState.user?.theme || 'system');
    system.addEventListener('change', schemeChanged);
    return () => {
      appState.session++;
      window.removeEventListener('hashchange', syncHash);
      system.removeEventListener('change', schemeChanged);
    };
  });
  $effect(() => {
    const page = navigation.find((item) => item.view === appState.view);
    document.title = appState.user
      ? `${t(page?.label || 'Translate')} · Subtitle Translator`
      : 'Subtitle Translator';
  });
</script>

{#if loading}
  <main class="startup">
    <Brand /><span class="loading-line" aria-label={t('Loading…')}></span>
  </main>
{:else if startupError}
  <main class="startup">
    <Brand />
    <p class="form-error" role="alert">{startupError}</p>
    <button class="button" onclick={initialize}>{t('Try again')}</button>
  </main>
{:else if !appState.user}
  <Auth onSignedIn={signedIn} />
  <div class="signed-out-locale">
    <Icon name="globe" size={15} /><select
      aria-label={t('Interface language')}
      value={appState.catalog.locale}
      onchange={changeLocale}
      >{#each Object.entries(locales) as [code, label]}<option value={code}>{label}</option
        >{/each}</select
    >
  </div>
{:else}
  <div class="app-shell">
    <aside class="sidebar">
      <div class="sidebar-brand">
        <Brand /><button
          class="icon-button mobile-logout"
          aria-label={t('Sign out')}
          onclick={logout}
          disabled={signingOut}><Icon name="logout" /></button
        >
      </div>
      <nav aria-label={t('Main navigation')}>
        {#each navigation.filter((item) => !item.admin || appState.user?.role === 'admin') as item}
          <a
            class="nav-item"
            class:active={appState.view === item.view}
            class:nav-separator={item.view === 'settings' ||
              (item.view === 'account' && appState.user?.role !== 'admin')}
            href={`#${item.view}`}
            aria-current={appState.view === item.view ? 'page' : undefined}
            ><Icon name={item.icon} size={19} /><span>{t(item.label)}</span></a
          >
        {/each}
      </nav>
      <div class="sidebar-bottom">
        <div class="preference-row">
          <Icon name="globe" size={16} /><select
            aria-label={t('Interface language')}
            value={appState.catalog.locale}
            onchange={changeLocale}
            >{#each Object.entries(locales) as [code, label]}<option value={code}>{label}</option
              >{/each}</select
          >
        </div>
        <div class="preference-row">
          <Icon name={appState.scheme === 'dark' ? 'moon' : 'sun'} size={16} /><select
            aria-label={t('Appearance')}
            value={appState.user.theme}
            onchange={saveTheme}
            disabled={themeBusy}
            ><option value="system">{t('System theme')}</option><option value="light"
              >{t('Light')}</option
            ><option value="dark">{t('Dark')}</option></select
          >
        </div>
        <div class="account-row">
          <button class="account-link" onclick={() => navigate('account')}
            ><span class="avatar">{appState.user.username.slice(0, 1).toUpperCase()}</span><span
              class="truncate">{appState.user.username}</span
            ></button
          ><button
            class="icon-button"
            aria-label={t('Sign out')}
            title={t('Sign out')}
            disabled={signingOut}
            onclick={logout}><Icon name="logout" size={17} /></button
          >
        </div>
      </div>
    </aside>
    <main class="workspace" id="main-content">
      {#key appState.session}
        {#if appState.view === 'translate'}
          <Translate onExpired={expired} />
        {:else if appState.view === 'history'}
          <header class="page-header"><h1>{t('History')}</h1></header>
          <JobList full onExpired={expired} />
        {:else if appState.view === 'settings' && appState.user.role === 'admin'}
          <SettingsView />
        {:else if appState.view === 'users' && appState.user.role === 'admin'}
          <Users />
        {:else if appState.view === 'account'}
          <Account />
        {/if}
      {/key}
    </main>
  </div>
{/if}
{#if appState.notice}<div class="toast" role="status">
    <Icon name="check" size={17} /><span>{appState.notice}</span><button
      class="icon-button"
      aria-label={t('Dismiss')}
      onclick={() => {
        appState.notice = '';
      }}><Icon name="close" size={15} /></button
    >
  </div>{/if}
