<script lang="ts">
  import { errorMessage } from '../lib/api';
  import {
    appState,
    languageName,
    navigate,
    notify,
    providerLabel,
    t,
  } from '../lib/appState.svelte';
  import { activeJobCount, loadJobs, loadUsers, RECENT_JOBS } from '../lib/workspace.svelte';

  const overview = $derived(appState.overview);
  const admin = $derived(appState.user?.role === 'admin');
  const active = $derived(activeJobCount(overview.counts));
  const providers = $derived(Object.entries(appState.settings?.providers || {}));
  const readyCount = $derived(
    providers.filter(([key]) => appState.settings?.configured[key]).length,
  );

  function formatJobDate(value: string): string {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return '';
    return new Intl.DateTimeFormat(appState.catalog.locale, {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(date);
  }
  function refresh() {
    Promise.all([loadJobs(), admin ? loadUsers() : Promise.resolve()]).catch((cause) =>
      notify(errorMessage(cause)),
    );
  }
</script>

<div class="welcome-row">
  <div>
    <h2 id="welcomeTitle">
      {t('Welcome back, {username}', { username: appState.user?.username || '' })}
    </h2>
    <p id="welcomeDescription">
      {admin
        ? t('Your panel-wide activity and access overview is ready.')
        : t('Here is what is happening in your translation workspace.')}
    </p>
  </div>
  <button class="ghost" type="button" id="dashboardRefresh" onclick={refresh}
    >{t('Refresh overview')}</button
  >
</div>
<div class="metric-grid" id="dashboardMetrics">
  <article class="metric-card">
    <span class="metric-label">{t('Total jobs')}</span>
    <strong class="metric-value">{overview.total}</strong>
    <span class="metric-note">{admin ? t('Across all users') : t('In your workspace')}</span>
  </article>
  <article class="metric-card" class:accent={active > 0}>
    <span class="metric-label">{t('Active now')}</span>
    <strong class="metric-value">{active}</strong>
    <span class="metric-note"
      >{active === 1 ? t('Translation in progress') : t('Translations in progress')}</span
    >
  </article>
  <article class="metric-card">
    <span class="metric-label">{t('Completed')}</span>
    <strong class="metric-value">{overview.counts.completed || 0}</strong>
    <span class="metric-note">{t('Ready or downloaded')}</span>
  </article>
  <article class="metric-card">
    <span class="metric-label">{t('Needs attention')}</span>
    <strong class="metric-value">{overview.counts.failed || 0}</strong>
    <span class="metric-note">{t('Failed translations')}</span>
  </article>
</div>
<div class="dashboard-grid">
  <section class="panel dashboard-card recent-card">
    <div class="card-heading">
      <div>
        <span class="eyebrow">{t('Activity')}</span>
        <h2>{t('Recent translations')}</h2>
      </div>
      <button
        class="text-button"
        type="button"
        onclick={() => {
          navigate('jobs');
        }}>{t('View all')} →</button
      >
    </div>
    <div class="recent-list" id="dashboardRecent">
      {#each overview.jobs.slice(0, RECENT_JOBS) as job (job.id)}
        <div class="recent-row">
          <div>
            <div class="recent-name" title={job.filename}>{job.filename}</div>
            <div class="recent-meta">
              {providerLabel(job.options.provider)} · {job.options.target_languages
                .map(languageName)
                .join(', ')} · {formatJobDate(job.created_at)}
            </div>
          </div>
          <span class={`status ${job.status}`}>{t(job.status)}</span>
        </div>
      {:else}
        <div class="empty">{t('No translations yet.')}</div>
      {/each}
    </div>
  </section>
  <aside class="panel dashboard-card quick-card">
    <span class="eyebrow">{t('Quick start')}</span>
    <h2>{t('Translate another subtitle')}</h2>
    <p>{t('Start with SRT, VTT, ASS, or SSA files. Timing and styling stay intact.')}</p>
    <button
      class="primary"
      type="button"
      onclick={() => {
        navigate('translate');
      }}>{t('Open translation workspace')} <span>→</span></button
    >
    <div class="provider-summary" id="dashboardProviders">
      {#if appState.settings}
        <div class="provider-summary-row">
          <span>{t('Providers ready')}</span><strong>{readyCount}/{providers.length}</strong>
        </div>
        <div class="provider-summary-row">
          <span>{t('Default provider')}</span><strong
            >{appState.settings.providers[String(appState.settings.default_provider)] ??
              appState.settings.default_provider}</strong
          >
        </div>
      {/if}
    </div>
  </aside>
</div>
