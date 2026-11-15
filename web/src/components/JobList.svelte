<script lang="ts">
  import { untrack } from 'svelte';
  import Icon from './Icon.svelte';
  import { api, ApiError, errorMessage, isAborted } from '../lib/api';
  import { languageName, notify, appState, t } from '../lib/appState.svelte';
  import type { Job, JobPage } from '../lib/types';

  let { full = false, onExpired }: { full?: boolean; onExpired: () => Promise<void> } = $props();
  let page = $state<JobPage>({
    jobs: [],
    total: 0,
    counts: {},
    offset: 0,
    limit: 30,
    has_more: false,
  });
  let offset = $state(0);
  let all = $state(false);
  let loading = $state(true);
  let loaded = $state(false);
  let error = $state('');
  let refresh = $state(0);
  let pending = $state<string[]>([]);
  let deleting = $state<Job | null>(null);
  let dialog: HTMLDialogElement;
  const activeStatuses = ['queued', 'processing', 'canceling'];
  function createdAt(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.valueOf())
      ? value
      : new Intl.DateTimeFormat(appState.catalog.locale, {
          month: 'short',
          day: 'numeric',
          hour: '2-digit',
          minute: '2-digit',
        }).format(date);
  }
  $effect(() => {
    const start = offset;
    const includeAll = all;
    const generation = appState.session;
    const limit = full ? 30 : 5;
    void refresh;
    void appState.jobsRevision;
    let alive = true;
    let inFlight = false;
    let failures = 0;
    let timer: ReturnType<typeof setTimeout>;
    const controller = new AbortController();
    async function load() {
      if (!alive || inFlight || document.hidden) return;
      inFlight = true;
      loading = true;
      try {
        const params = new URLSearchParams({ limit: String(limit), offset: String(start) });
        if (includeAll && full) params.set('all', '1');
        const data = await api<JobPage>(`/api/jobs?${params}`, { signal: controller.signal });
        if (!alive || generation !== appState.session) return;
        page = data;
        loaded = true;
        error = '';
        failures = 0;
        if (start > 0 && data.jobs.length === 0) offset = Math.max(0, start - limit);
      } catch (cause) {
        if (!alive || generation !== appState.session || isAborted(cause)) return;
        if (cause instanceof ApiError && cause.status === 401) {
          try {
            await api('/api/auth/me', { signal: controller.signal });
          } catch (verification) {
            if (
              alive &&
              generation === appState.session &&
              verification instanceof ApiError &&
              verification.status === 401
            ) {
              await onExpired();
              return;
            }
          }
        }
        if (alive) {
          error = errorMessage(cause);
          failures++;
        }
      } finally {
        inFlight = false;
        if (alive && generation === appState.session) {
          loading = false;
          const active = page.jobs.some((job) => activeStatuses.includes(job.status));
          const wait = failures
            ? Math.min(60000, 3000 * 2 ** Math.min(failures - 1, 5))
            : active
              ? 1500
              : 10000;
          timer = setTimeout(() => {
            void load();
          }, wait);
        }
      }
    }
    const visible = () => {
      clearTimeout(timer);
      if (!document.hidden) void load();
    };
    document.addEventListener('visibilitychange', visible);
    untrack(() => {
      void load();
    });
    return () => {
      alive = false;
      clearTimeout(timer);
      controller.abort();
      document.removeEventListener('visibilitychange', visible);
    };
  });
  $effect(() => {
    if (deleting && dialog && !dialog.open) dialog.showModal();
  });
  async function action(job: Job, operation: 'cancel' | 'delete') {
    pending = [...pending, job.id];
    const generation = appState.session;
    try {
      await api(
        `/api/jobs/${encodeURIComponent(job.id)}${operation === 'cancel' ? '/cancel' : ''}`,
        { method: operation === 'cancel' ? 'POST' : 'DELETE' },
      );
      if (generation !== appState.session) return;
      deleting = null;
      dialog?.close();
      refresh++;
      notify(operation === 'cancel' ? t('Cancellation requested') : t('Job deleted'));
    } catch (cause) {
      if (generation === appState.session) error = errorMessage(cause);
    } finally {
      if (generation === appState.session) pending = pending.filter((id) => id !== job.id);
    }
  }
  function downloadUrl(job: Job, name?: string): string {
    return `/api/jobs/${encodeURIComponent(job.id)}/download${name ? `/${encodeURIComponent(name)}` : ''}`;
  }
</script>

{#if full}
  <div class="history-toolbar">
    <span class="muted">{t('{count} translations', { count: page.total })}</span>
    <div class="toolbar-actions">
      {#if appState.user?.role === 'admin'}<label class="inline-checkbox"
          ><input
            type="checkbox"
            bind:checked={all}
            onchange={() => {
              offset = 0;
            }}
          />{t('All users')}</label
        >{/if}<button
        class="button small"
        type="button"
        disabled={loading}
        onclick={() => {
          refresh++;
        }}><Icon name="refresh" size={15} />{t('Refresh')}</button
      >
    </div>
  </div>
{/if}
{#if error}<div class="inline-error" role="alert">
    <span>{error}</span><button
      type="button"
      class="text-button"
      onclick={() => {
        refresh++;
      }}>{t('Try again')}</button
    >
  </div>{/if}
<div class="jobs-panel" aria-busy={loading && !loaded}>
  {#if !loaded && loading}<div class="empty-state">
      <span class="loading-line"></span><span class="muted">{t('Loading translations…')}</span>
    </div>
  {:else if page.jobs.length === 0}
    <div class="empty-state">
      <Icon name="history" size={26} />
      <p>{t('No translations yet.')}</p>
      {#if full}<a class="text-button" href="#translate"
          >{t('New translation')}<Icon name="arrow" size={14} /></a
        >{/if}
    </div>
  {:else}
    <div class="job-table-heading" aria-hidden="true">
      <span>{t('File')}</span><span>{t('Status')}</span><span>{t('Actions')}</span>
    </div>
    <ul class="job-list">
      {#each page.jobs as job (job.id)}
        <li class="job-row">
          <div class="job-main">
            <span class="job-file-icon"><Icon name="file" size={20} /></span>
            <div class="job-file-info">
              <h3 class="job-name" title={job.filename}>{job.filename}</h3>
              <div class="job-metadata">
                <span
                  >{appState.settings?.providers[job.options.provider] ||
                    job.options.provider}</span
                ><span class="metadata-dot">·</span><span
                  >{job.options.target_languages.map(languageName).join(', ')}</span
                >{#if job.owner !== undefined}<span class="metadata-dot">·</span><span
                    >{job.owner || t('deleted user')}</span
                  >{/if}
              </div>
              {#if full}<time class="job-date" datetime={job.created_at}
                  >{createdAt(job.created_at)}</time
                >{/if}
            </div>
          </div>
          <div class="job-state">
            <div class="status-line">
              <span class={`status-dot ${job.status}`}></span><span>{t(job.status)}</span
              >{#if activeStatuses.includes(job.status)}<span class="progress-number"
                  >{Math.min(100, Math.max(0, job.progress))}%</span
                >{/if}
            </div>
            {#if activeStatuses.includes(job.status)}<progress
                max="100"
                value={job.progress}
                aria-label={t('Translation progress for {filename}', { filename: job.filename })}
              ></progress><span class="job-stage">{job.stage}</span>{/if}
          </div>
          <div class="job-actions">
            {#if job.status === 'completed' && job.outputs.length}
              {#if job.outputs.length === 1}<a
                  class="button small download-button"
                  href={downloadUrl(job)}><Icon name="download" size={15} />{t('Download')}</a
                >
              {:else}<details class="download-menu">
                  <summary class="button small"
                    ><Icon name="download" size={15} />{t('Download')}<Icon
                      name="down"
                      size={13}
                    /></summary
                  >
                  <div class="download-options">
                    <a href={downloadUrl(job)}>{t('Download ZIP')}</a
                    >{#each job.outputs as output}<a href={downloadUrl(job, output.name)}
                        >{languageName(output.language)}</a
                      >{/each}
                  </div>
                </details>{/if}
            {/if}
            {#if job.status === 'queued' || job.status === 'processing'}<button
                class="button quiet small"
                type="button"
                disabled={pending.includes(job.id)}
                onclick={() => action(job, 'cancel')}>{t('Cancel')}</button
              >
            {:else if job.status === 'canceling'}<span class="muted small-text"
                >{t('Canceling…')}</span
              >
            {:else}<button
                class="icon-button delete-job"
                type="button"
                disabled={pending.includes(job.id)}
                title={t('Delete')}
                aria-label={t('Delete {filename}', { filename: job.filename })}
                onclick={() => {
                  deleting = job;
                }}><Icon name="trash" size={16} /></button
              >{/if}
          </div>
          {#if job.error}<p class="job-error">{job.error}</p>{/if}
          {#if job.warning}<p class="job-warning">{job.warning}</p>{/if}
        </li>
      {/each}
    </ul>
  {/if}
</div>
{#if full && (offset > 0 || page.has_more)}<nav
    class="pagination"
    aria-label={t('Translation history pages')}
  >
    <button
      class="button small"
      disabled={offset === 0 || loading}
      onclick={() => {
        offset = Math.max(0, offset - 30);
      }}>{t('Newer')}</button
    ><output
      >{t('Showing {start}–{end} of {total}', {
        start: offset + 1,
        end: offset + page.jobs.length,
        total: page.total,
      })}</output
    ><button
      class="button small"
      disabled={!page.has_more || loading}
      onclick={() => {
        offset += 30;
      }}>{t('Older')}</button
    >
  </nav>{/if}
<dialog
  bind:this={dialog!}
  class="confirm-dialog"
  onclose={() => {
    deleting = null;
  }}
>
  {#if deleting}<h2>{t('Delete translation?')}</h2>
    <p class="muted">{t('The source file and translated outputs will be permanently deleted.')}</p>
    <p class="confirm-filename">{deleting.filename}</p>
    <div class="dialog-actions">
      <button
        class="button"
        type="button"
        disabled={pending.includes(deleting.id)}
        onclick={() => {
          dialog.close();
        }}>{t('Keep translation')}</button
      ><button
        class="button danger"
        type="button"
        disabled={pending.includes(deleting.id)}
        onclick={() => {
          if (deleting) void action(deleting, 'delete');
        }}>{t('Delete')}</button
      >
    </div>{/if}
</dialog>
