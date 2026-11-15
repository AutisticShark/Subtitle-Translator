import type { Catalog, Settings, SetupStatus, Theme, User, View } from './types';

export const appState = $state({
  user: null as User | null,
  settings: null as Settings | null,
  auth: null as SetupStatus | null,
  catalog: { locale: 'en', messages: {}, languages: {} } as Catalog,
  view: 'translate' as View,
  session: 0,
  jobsRevision: 0,
  notice: '',
  scheme: 'light' as 'light' | 'dark',
});

let noticeTimer: ReturnType<typeof setTimeout>;
export function notify(message: string) {
  appState.notice = message;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => {
    appState.notice = '';
  }, 5000);
}

export function t(message: string, values: Record<string, string | number> = {}): string {
  return (appState.catalog.messages[message] || message).replace(
    /\{([a-z_]+)\}/gi,
    (match: string, key: string) => (Object.hasOwn(values, key) ? String(values[key]) : match),
  );
}

export function setting(key: string, fallback = ''): string {
  return String(appState.settings?.[key] ?? fallback);
}

export function languageName(code: string): string {
  return appState.catalog.languages[code] || code;
}

export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
  appState.scheme =
    theme === 'system'
      ? window.matchMedia('(prefers-color-scheme: dark)').matches
        ? 'dark'
        : 'light'
      : theme;
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute('content', appState.scheme === 'dark' ? '#14171c' : '#f7f8fa');
}

export function navigate(view: View) {
  appState.view = view;
  window.location.hash = view;
}

export function clearSession() {
  appState.session += 1;
  appState.user = null;
  appState.settings = null;
  appState.notice = '';
  appState.view = 'translate';
  applyTheme('system');
}
