import type { Catalog, JobPage, Settings, SetupStatus, Theme, User, View } from './types';

export const views: View[] = ['dashboard', 'translate', 'jobs', 'security', 'admin'];

export function emptyJobPage(): JobPage {
  return { jobs: [], total: 0, counts: {}, offset: 0, limit: 50, has_more: false };
}

export const appState = $state({
  user: null as User | null,
  settings: null as Settings | null,
  auth: null as SetupStatus | null,
  catalog: { locale: 'en', messages: {}, languages: {} } as Catalog,
  view: 'dashboard' as View,
  session: 0,
  notice: '',
  noticeShown: false,
  scheme: 'light' as 'light' | 'dark',
  // history is the visible page; overview feeds the dashboard metrics and recent list.
  history: emptyJobPage(),
  overview: emptyJobPage(),
  jobsOffset: 0,
  allJobs: false,
  users: [] as User[],
});

let noticeTimer: ReturnType<typeof setTimeout>;
export function notify(message: string) {
  appState.notice = message;
  appState.noticeShown = true;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => {
    appState.noticeShown = false;
  }, 3000);
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

export function providerLabel(key: string): string {
  return appState.settings?.providers?.[key] || key;
}

export function applyTheme(theme: Theme) {
  const selected: Theme = ['system', 'light', 'dark'].includes(theme) ? theme : 'system';
  document.documentElement.dataset.theme = selected;
  appState.scheme =
    selected === 'system'
      ? window.matchMedia('(prefers-color-scheme: dark)').matches
        ? 'dark'
        : 'light'
      : selected;
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute('content', appState.scheme === 'dark' ? '#0c0e12' : '#f5f5f0');
}

export function showView(requested: string, updateHash = true) {
  const allowed = views.filter((view) => view !== 'admin' || appState.user?.role === 'admin');
  const view = allowed.find((item) => item === requested) ?? 'dashboard';
  appState.view = view;
  if (updateHash) window.history.replaceState(null, '', `#${view}`);
  window.scrollTo({ top: 0, behavior: 'smooth' });
}

export function navigate(view: View) {
  showView(view);
}

// Remove every trace of the previous account from memory.
export function clearSession() {
  appState.session += 1;
  appState.user = null;
  appState.settings = null;
  appState.notice = '';
  appState.noticeShown = false;
  appState.view = 'dashboard';
  appState.history = emptyJobPage();
  appState.overview = emptyJobPage();
  appState.jobsOffset = 0;
  appState.allJobs = false;
  appState.users = [];
  applyTheme('system');
}
