import { appState, t } from './appState.svelte';

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public data: Record<string, unknown> = {},
  ) {
    super(message);
    this.name = 'ApiError';
  }
}
export class StaleSession extends Error {}

function csrfToken(): string {
  const value = document.cookie.split('; ').find((item) => item.startsWith('csrf_access_token='));
  return value ? decodeURIComponent(value.slice('csrf_access_token='.length)) : '';
}

export async function api<T>(url: string, options: RequestInit = {}): Promise<T> {
  const method = (options.method || 'GET').toUpperCase();
  const headers = new Headers(options.headers);
  if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(method)) {
    const csrf = csrfToken();
    if (csrf) headers.set('X-CSRF-TOKEN', csrf);
  }
  const response = await fetch(url, { ...options, method, headers, credentials: 'same-origin' });
  const data = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok)
    throw new ApiError(
      response.status,
      typeof data.error === 'string'
        ? data.error
        : t('Request failed ({status})', { status: response.status }),
      data,
    );
  return data as T;
}

export function json<T>(url: string, body: unknown, method = 'POST'): Promise<T> {
  return api<T>(url, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

export async function sessionApi<T>(url: string, options: RequestInit = {}): Promise<T> {
  const generation = appState.session;
  try {
    const data = await api<T>(url, options);
    if (generation !== appState.session) throw new StaleSession();
    return data;
  } catch (error) {
    if (generation !== appState.session) throw new StaleSession();
    throw error;
  }
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : t('Something went wrong');
}

export function isAborted(error: unknown): boolean {
  return (
    error instanceof StaleSession || (error instanceof DOMException && error.name === 'AbortError')
  );
}
