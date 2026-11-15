import { api, ApiError, errorMessage, isAborted, sessionApi } from './api';
import { appState, emptyJobPage, notify } from './appState.svelte';
import type { JobPage, User } from './types';

export const JOBS_PAGE_SIZE = 50;
export const RECENT_JOBS = 5;
const POLL_ACTIVE_MS = 1500;
const POLL_IDLE_MS = 8000;
const POLL_MAX_BACKOFF_MS = 60000;
const activeStatuses = ['queued', 'processing', 'canceling'] as const;

let timer: ReturnType<typeof setTimeout> | undefined;
let controller: AbortController | null = null;
let request = 0;
let failures = 0;
let deferred = false;
let onExpired: (() => Promise<void>) | null = null;

export function activeJobCount(counts: JobPage['counts'] = {}): number {
  return activeStatuses.reduce((sum, status) => sum + (counts[status] || 0), 0);
}

function jobsUrl(params: Record<string, string>): string {
  return `/api/jobs?${new URLSearchParams(params)}`;
}

function schedule(delay: number) {
  clearTimeout(timer);
  timer = setTimeout(() => {
    // Every authenticated request may refresh the session cookie, so an unattended
    // background tab must not poll; resume when it is shown again.
    if (document.hidden) deferred = true;
    else void loadJobs();
  }, delay);
}

function visibilityChanged() {
  if (document.hidden || !deferred) return;
  deferred = false;
  void loadJobs();
}

// A request sent with a cookie that an MFA change just rotated fails with 401
// although the session continues; only a 401 from /me means signed out.
async function sessionEnded(isCurrent: () => boolean): Promise<boolean> {
  try {
    await api('/api/auth/me');
    return false;
  } catch (cause) {
    return isCurrent() && cause instanceof ApiError && cause.status === 401;
  }
}

export async function loadJobs(): Promise<void> {
  const user = appState.user;
  if (!user) return; // Never poll while signed out.
  const session = appState.session;
  const id = ++request;
  controller?.abort(); // A newer load (scope toggle, page, refresh) wins.
  const own = new AbortController();
  controller = own;
  clearTimeout(timer);
  const current = () => session === appState.session && id === request;
  try {
    const admin = user.role === 'admin';
    const all = admin && appState.allJobs;
    const historyParams: Record<string, string> = {
      limit: String(JOBS_PAGE_SIZE),
      offset: String(appState.jobsOffset),
    };
    if (all) historyParams.all = '1';
    // Administrators always see panel-wide dashboard metrics.
    const reuse = appState.jobsOffset === 0 && all === admin;
    const [history, overview] = await Promise.all([
      api<JobPage>(jobsUrl(historyParams), { signal: own.signal }),
      reuse
        ? null
        : api<JobPage>(jobsUrl({ limit: String(RECENT_JOBS), ...(admin ? { all: '1' } : {}) }), {
            signal: own.signal,
          }),
    ]);
    if (!current()) return;
    if (!history.jobs.length && history.offset > 0) {
      // The page emptied (deletions); step back to the last page that has jobs.
      appState.jobsOffset = Math.max(
        0,
        Math.floor((history.total - 1) / JOBS_PAGE_SIZE) * JOBS_PAGE_SIZE,
      );
      if (appState.jobsOffset < history.offset) return loadJobs();
    }
    failures = 0;
    appState.jobsOffset = history.offset;
    appState.history = history;
    appState.overview = overview || history;
    const active =
      activeJobCount(history.counts) > 0 || activeJobCount(appState.overview.counts) > 0;
    schedule(active ? POLL_ACTIVE_MS : POLL_IDLE_MS);
  } catch (cause) {
    if (!current() || isAborted(cause)) return; // Superseded, aborted, or a previous session.
    if (cause instanceof ApiError && cause.status === 401) {
      if (await sessionEnded(current)) {
        // Expired (including the absolute session lifetime), revoked, or signed out elsewhere.
        await onExpired?.();
        return;
      }
      if (current()) schedule(POLL_ACTIVE_MS);
      return;
    }
    failures += 1;
    if (failures === 1) notify(errorMessage(cause));
    schedule(Math.min(POLL_IDLE_MS * 2 ** (failures - 1), POLL_MAX_BACKOFF_MS));
  } finally {
    if (controller === own) controller = null;
  }
}

export function showJobsPage(offset: number) {
  appState.jobsOffset = Math.max(0, offset);
  void loadJobs();
}

export async function loadUsers(): Promise<void> {
  try {
    const data = await sessionApi<{ users: User[] }>('/api/users');
    appState.users = data.users;
  } catch (cause) {
    if (!isAborted(cause)) throw cause;
  }
}

// Start a new polling generation for the signed-in account.
export function startWorkspace(expired: () => Promise<void>) {
  stopWorkspace();
  onExpired = expired;
  const admin = appState.user?.role === 'admin';
  appState.jobsOffset = 0;
  appState.allJobs = admin;
  appState.history = emptyJobPage();
  appState.overview = emptyJobPage();
  document.addEventListener('visibilitychange', visibilityChanged);
  void loadJobs();
  if (admin) void loadUsers().catch((cause) => notify(errorMessage(cause)));
}

// Stop polling and make in-flight responses stale (sign-out, session switch, unmount).
export function stopWorkspace() {
  request += 1;
  controller?.abort();
  controller = null;
  clearTimeout(timer);
  timer = undefined;
  failures = 0;
  deferred = false;
  onExpired = null;
  document.removeEventListener('visibilitychange', visibilityChanged);
}
