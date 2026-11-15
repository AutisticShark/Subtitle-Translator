<script lang="ts">
  import { api, errorMessage } from '../lib/api';
  import { appState, languageName, notify, providerLabel, t } from '../lib/appState.svelte';
  import type { Job } from '../lib/types';
  import { JOBS_PAGE_SIZE, loadJobs, showJobsPage } from '../lib/workspace.svelte';

  let pending = $state<string[]>([]);
  const history = $derived(appState.history);
  const admin = $derived(appState.user?.role === 'admin');

  function jobStatusText(job: Job): string {
    // Stages arrive localized; a queued job has no stage yet, so localize its status.
    return job.stage || t(job.status);
  }
  function downloadUrl(job: Job, name?: string): string {
    return `/api/jobs/${encodeURIComponent(job.id)}/download${name ? `/${encodeURIComponent(name)}` : ''}`;
  }
  async function act(job: Job, operation: 'cancel' | 'delete') {
    if (
      !window.confirm(
        operation === 'cancel'
          ? t('Cancel {filename}?', { filename: job.filename })
          : t('Delete {filename}?', { filename: job.filename }),
      )
    )
      return;
    const generation = appState.session;
    pending = [...pending, job.id];
    try {
      if (operation === 'cancel') {
        await api(`/api/jobs/${encodeURIComponent(job.id)}/cancel`, { method: 'POST' });
      } else {
        await api(`/api/jobs/${encodeURIComponent(job.id)}`, { method: 'DELETE' });
      }
      if (generation !== appState.session) return;
      await loadJobs();
      notify(operation === 'cancel' ? t('Cancellation requested') : t('Translation deleted'));
    } catch (cause) {
      if (generation === appState.session) notify(errorMessage(cause));
    } finally {
      if (generation === appState.session) pending = pending.filter((id) => id !== job.id);
    }
  }
</script>

<div class="section-heading">
  <div>
    <span class="eyebrow">{t('Workspace')}</span>
    <h2>{t('Translation history')}</h2>
    <p>{t('Monitor active work and download completed subtitles.')}</p>
  </div>
  <div class="section-actions">
    {#if admin}<label class="toggle-label" id="allJobsLabel"
        ><input
          id="allJobs"
          type="checkbox"
          checked={appState.allJobs}
          onchange={(event) => {
            appState.allJobs = event.currentTarget.checked;
            showJobsPage(0);
          }}
        />
        {t('All users')}</label
      >{/if}
    <button class="ghost small" id="refreshButton" type="button" onclick={() => loadJobs()}
      >{t('Refresh')}</button
    >
  </div>
</div>
<div id="jobs" class="jobs">
  {#each appState.history.jobs as job (job.id)}
    <article class="job">
      <div>
        <div class="job-name" title={job.filename}>{job.filename}</div>
        <div class="job-meta">
          {providerLabel(job.options.provider)} · {job.options.target_languages
            .map(languageName)
            .join(', ')}{#if job.owner !== undefined}{' · '}{job.owner || t('deleted user')}{/if}
        </div>
      </div>
      <div>
        <div
          class="progress-track"
          role="progressbar"
          aria-label={job.filename}
          aria-valuemin="0"
          aria-valuemax="100"
          aria-valuenow={Number(job.progress)}
        >
          <div class="progress-bar" style:width={`${Number(job.progress)}%`}></div>
        </div>
        <div class="job-meta">{jobStatusText(job)} · {Number(job.progress)}%</div>
        {#if job.error}<div class="job-error">{job.error}</div>{/if}
        {#if job.warning}<div class="job-warning">{job.warning}</div>{/if}
      </div>
      <div class="job-actions">
        {#if job.status === 'completed'}
          <div class="download-actions">
            <a class="download" href={downloadUrl(job)}
              >{t(job.outputs.length > 1 ? 'Download ZIP' : 'Download')}</a
            >
            {#if job.outputs.length > 1}
              <div class="language-downloads">
                {#each job.outputs as output (output.name)}<a href={downloadUrl(job, output.name)}
                    >{languageName(output.language)}</a
                  >{/each}
              </div>
            {/if}
          </div>
        {:else}
          <span class={`status ${job.status}`}>{t(job.status)}</span>
        {/if}
        {#if job.status === 'queued' || job.status === 'processing'}
          <button
            class="cancel-job"
            type="button"
            disabled={pending.includes(job.id)}
            onclick={() => act(job, 'cancel')}>{t('Cancel')}</button
          >
        {:else if job.status === 'canceling'}
          <button class="cancel-job" type="button" disabled>{t('Canceling…')}</button>
        {/if}
        {#if job.status === 'completed' || job.status === 'failed' || job.status === 'canceled'}
          <button
            class="delete-job"
            type="button"
            disabled={pending.includes(job.id)}
            onclick={() => act(job, 'delete')}>{t('Delete')}</button
          >
        {/if}
      </div>
    </article>
  {:else}
    <div class="empty">{t('No translations yet.')}</div>
  {/each}
</div>
<nav
  class="section-actions jobs-pager"
  id="jobsPager"
  aria-label={t('Translation history pages')}
  hidden={history.offset === 0 && !history.has_more}
>
  <button
    class="ghost small"
    id="jobsNewer"
    type="button"
    disabled={history.offset === 0}
    onclick={() => showJobsPage(appState.jobsOffset - JOBS_PAGE_SIZE)}>← {t('Newer')}</button
  >
  <output id="jobsPageStatus"
    >{history.jobs.length
      ? t('Showing {start}–{end} of {total}', {
          start: history.offset + 1,
          end: history.offset + history.jobs.length,
          total: history.total,
        })
      : ''}</output
  >
  <button
    class="ghost small"
    id="jobsOlder"
    type="button"
    disabled={!history.has_more}
    onclick={() => showJobsPage(appState.jobsOffset + JOBS_PAGE_SIZE)}>{t('Older')} →</button
  >
</nav>
