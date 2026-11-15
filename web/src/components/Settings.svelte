<script lang="ts">
  import { onDestroy } from 'svelte';
  import Icon from './Icon.svelte';
  import { api, errorMessage, json } from '../lib/api';
  import { notify, appState, t } from '../lib/appState.svelte';
  import type { Settings, SetupStatus } from '../lib/types';

  let section = $state('providers');
  let draft = $state<Record<string, string>>({});
  let secrets = $state<Record<string, string>>({});
  let busy = $state(false);
  let error = $state('');
  let dirty = $state(false);
  let removeProvider = $state('');
  let dialog: HTMLDialogElement;
  const sections = [
    ['providers', 'Providers'],
    ['translation', 'Translation'],
    ['limits', 'Limits'],
    ['access', 'Access'],
  ];
  const providerKeys = [
    { code: 'anthropic', label: 'Anthropic', secret: 'anthropic_api_key' },
    { code: 'openai', label: 'OpenAI-compatible', secret: 'openai_api_key' },
    { code: 'deepl', label: 'DeepL', secret: 'deepl_api_key' },
    { code: 'google', label: 'Google Cloud Translation', secret: 'google_api_key' },
  ];
  const captchas = [
    ['turnstile', 'Cloudflare Turnstile'],
    ['recaptcha', 'Google reCAPTCHA v2'],
    ['hcaptcha', 'hCaptcha'],
  ];
  const scopes = [
    ['user', 'Users'],
    ['admin', 'Administrators'],
    ['panel', 'Panel'],
  ];
  const periods = [
    ['', 'Window'],
    ['daily', 'Daily'],
    ['weekly', 'Weekly'],
    ['monthly', 'Monthly'],
  ];
  const tuning = [
    { key: 'batch_size', label: 'Batch size', min: 1, max: 100, step: 1 },
    { key: 'workers', label: 'API workers', min: 1, max: 16, step: 1 },
    { key: 'rpm', label: 'Requests per minute', min: 0, max: 10000, step: 'any' },
    { key: 'width', label: 'Line width', min: 4, max: 80, step: 'any' },
    { key: 'max_lines', label: 'Maximum lines', min: 1, max: 5, step: 1 },
  ];
  function reset() {
    draft = Object.fromEntries(
      Object.entries(appState.settings || {}).filter(([, value]) => typeof value === 'string'),
    ) as Record<string, string>;
    secrets = {};
    dirty = false;
    error = '';
  }
  reset();
  onDestroy(() => {
    secrets = {};
  });
  function limitKey(scope: string, period: string): string {
    return `${scope}_${period ? `${period}_` : ''}job_limit`;
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = '';
    const generation = appState.session;
    try {
      const result = await json<Settings>(
        '/api/settings',
        {
          ...draft,
          ...Object.fromEntries(Object.entries(secrets).filter(([, value]) => value.trim())),
        },
        'PUT',
      );
      if (generation !== appState.session) return;
      appState.settings = result;
      appState.auth = await api<SetupStatus>('/api/auth/setup-status');
      reset();
      notify(t('Settings saved'));
    } catch (cause) {
      if (generation === appState.session) error = errorMessage(cause);
    } finally {
      secrets = {};
      if (generation === appState.session) busy = false;
    }
  }
  $effect(() => {
    if (removeProvider && dialog && !dialog.open) dialog.showModal();
  });
  async function removeKey() {
    busy = true;
    error = '';
    try {
      appState.settings = await api<Settings>(
        `/api/settings/keys/${encodeURIComponent(removeProvider)}`,
        { method: 'DELETE' },
      );
      secrets = {};
      dialog.close();
      notify(t('Key removed'));
    } catch (cause) {
      error = errorMessage(cause);
      dialog.close();
    } finally {
      busy = false;
    }
  }
</script>

<header class="page-header">
  <h1>{t('Settings')}</h1>
  <button class="primary" form="panel-settings" type="submit" disabled={busy || !dirty}
    >{busy ? t('Saving…') : t('Save changes')}</button
  >
</header>
<div class="settings-tabs" role="tablist" aria-label={t('Settings sections')}>
  {#each sections as [id, label]}<button
      type="button"
      role="tab"
      id={`tab-${id}`}
      aria-controls="settings-panel"
      aria-selected={section === id}
      tabindex={section === id ? 0 : -1}
      class:active={section === id}
      onclick={() => {
        section = id;
      }}
      onkeydown={(event) => {
        if (['ArrowRight', 'ArrowLeft', 'Home', 'End'].includes(event.key)) {
          event.preventDefault();
          const current = sections.findIndex((item) => item[0] === section);
          const next =
            event.key === 'Home'
              ? 0
              : event.key === 'End'
                ? sections.length - 1
                : (current + (event.key === 'ArrowRight' ? 1 : -1) + sections.length) %
                  sections.length;
          section = sections[next][0];
          document.getElementById(`tab-${section}`)?.focus();
        }
      }}>{t(label)}</button
    >{/each}
</div>
{#if error}<p class="inline-error" role="alert">{error}</p>{/if}
<form
  id="panel-settings"
  class="settings-form"
  onsubmit={save}
  onchange={() => {
    dirty = true;
  }}
  oninput={() => {
    dirty = true;
  }}
>
  <div id="settings-panel" role="tabpanel" aria-labelledby={`tab-${section}`}>
    {#if section === 'providers'}
      <section class="settings-section">
        <h2>{t('Default provider')}</h2>
        <div class="settings-fields">
          <label class="visually-hidden" for="default-provider">{t('Default provider')}</label
          ><select id="default-provider" bind:value={draft.default_provider}
            >{#each Object.entries(appState.settings?.providers || {}) as [code, label]}<option
                value={code}>{label}</option
              >{/each}</select
          >
        </div>
      </section>
      {#each providerKeys as provider}
        <section class="settings-section">
          <div class="settings-section-title">
            <h2>{provider.label}</h2>
            <span class="key-state" class:configured={appState.settings?.configured[provider.code]}
              ><span></span>{appState.settings?.configured[provider.code]
                ? t('Configured')
                : t('Not set')}</span
            >
          </div>
          <div class="settings-fields stack">
            <label
              >{t('API key')}
              <div class="key-input">
                <input
                  type="password"
                  autocomplete="new-password"
                  bind:value={secrets[provider.secret]}
                  placeholder={t('Leave blank to keep current key')}
                  aria-label={`${provider.label} ${t('API key')}`}
                />{#if appState.settings?.configured[provider.code]}<button
                    class="button quiet small"
                    type="button"
                    disabled={busy}
                    onclick={() => {
                      removeProvider = provider.code;
                    }}>{t('Remove')}</button
                  >{/if}
              </div></label
            >
            {#if provider.code === 'anthropic' || provider.code === 'openai'}<label
                >{t('Model')}<input
                  bind:value={draft[`${provider.code}_model`]}
                  maxlength="200"
                /></label
              >{/if}
            {#if provider.code === 'openai'}<label
                >{t('Base URL')}<input
                  bind:value={draft.openai_base_url}
                  type="url"
                  placeholder="https://api.openai.com/v1"
                /></label
              >{/if}
          </div>
        </section>
      {/each}
    {:else if section === 'translation'}
      <section class="settings-section">
        <h2>{t('Languages')}</h2>
        <div class="settings-fields stack">
          <label
            >{t('Source language')}<input
              bind:value={draft.source_language}
              maxlength="100"
            /></label
          ><label
            >{t('Default target languages')}<input
              bind:value={draft.target_languages}
              placeholder="zh-TW,zh-CN"
            /></label
          >
        </div>
      </section>
      <section class="settings-section">
        <h2>{t('Processing')}</h2>
        <div class="settings-fields tuning-grid">
          {#each tuning as field}<label
              >{t(field.label)}<input
                type="number"
                value={draft[field.key]}
                oninput={(event) => {
                  draft[field.key] = event.currentTarget.value;
                }}
                min={field.min}
                max={field.max}
                step={field.step}
                required
              /></label
            >{/each}
        </div>
      </section>
    {:else if section === 'limits'}
      <section class="settings-section">
        <h2>{t('Submission window')}</h2>
        <div class="settings-fields">
          <label
            >{t('Window length (minutes)')}<input
              type="number"
              value={draft.rate_limit_window_minutes}
              oninput={(event) => {
                draft.rate_limit_window_minutes = event.currentTarget.value;
              }}
              min="1"
              max="10080"
              step="1"
              required
            /></label
          >
        </div>
      </section>
      <section class="settings-section limits-section">
        <div>
          <h2>{t('Job limits')}</h2>
          <p class="muted settings-note">{t('0 means unlimited. Calendar periods use UTC.')}</p>
        </div>
        <div class="limit-table-wrap">
          <table class="limit-table">
            <thead
              ><tr
                ><th></th>{#each scopes as [, label]}<th scope="col">{t(label)}</th>{/each}</tr
              ></thead
            ><tbody
              >{#each periods as [period, label]}<tr
                  ><th scope="row">{t(label)}</th>{#each scopes as [scope, scopeLabel]}<td
                      ><input
                        type="number"
                        value={draft[limitKey(scope, period)]}
                        oninput={(event) => {
                          draft[limitKey(scope, period)] = event.currentTarget.value;
                        }}
                        min="0"
                        max={scope === 'panel' ? 1000000 : 100000}
                        step="1"
                        required
                        aria-label={`${t(scopeLabel)}: ${t(label)}`}
                      /></td
                    >{/each}</tr
                >{/each}</tbody
            >
          </table>
        </div>
      </section>
    {:else if section === 'access'}
      <section class="settings-section">
        <h2>{t('Registration')}</h2>
        <div class="settings-fields">
          <label class="inline-checkbox"
            ><input
              type="checkbox"
              checked={draft.registration_enabled === '1'}
              onchange={(event) => {
                draft.registration_enabled = event.currentTarget.checked ? '1' : '0';
              }}
            />{t('Allow new user sign-up')}</label
          >
        </div>
      </section>
      <section class="settings-section">
        <h2>CAPTCHA</h2>
        <div class="settings-fields stack">
          <label
            >{t('Provider')}<select bind:value={draft.captcha_provider}
              ><option value="none">{t('Disabled')}</option>{#each captchas as [code, label]}<option
                  value={code}>{label}</option
                >{/each}</select
            ></label
          >
          {#if draft.captcha_provider !== 'none'}
            <label
              >{t('Expected hostname')}<input
                bind:value={draft.captcha_hostname}
                placeholder="translate.example.com"
                required
              /></label
            >
            <label
              >{t('Site key')}<input
                bind:value={draft[`${draft.captcha_provider}_site_key`]}
                autocomplete="off"
              /></label
            >
            <label
              >{t('Secret key')}
              <div class="key-input">
                <input
                  type="password"
                  autocomplete="new-password"
                  bind:value={secrets[`${draft.captcha_provider}_secret_key`]}
                  placeholder={appState.settings?.captcha_configured[draft.captcha_provider]
                    ? t('Leave blank to keep current key')
                    : ''}
                />{#if appState.settings?.captcha_configured[draft.captcha_provider]}<button
                    class="button quiet small"
                    type="button"
                    onclick={() => {
                      removeProvider = `captcha-${draft.captcha_provider}`;
                    }}>{t('Remove')}</button
                  >{/if}
              </div></label
            >
            <div class="checkbox-stack">
              {#each [['login', 'Protect login'], ['register', 'Protect registration'], ['upload', 'Protect uploads']] as [action, label]}<label
                  class="inline-checkbox"
                  ><input
                    type="checkbox"
                    checked={draft[`captcha_on_${action}`] === '1'}
                    onchange={(event) => {
                      draft[`captcha_on_${action}`] = event.currentTarget.checked ? '1' : '0';
                    }}
                  />{t(label)}</label
                >{/each}
            </div>
          {/if}
        </div>
      </section>
    {/if}
  </div>
  {#if dirty}<div class="settings-bottom-actions">
      <button class="button quiet" type="button" disabled={busy} onclick={reset}
        >{t('Discard changes')}</button
      ><button class="primary" type="submit" disabled={busy}
        >{busy ? t('Saving…') : t('Save changes')}</button
      >
    </div>{/if}
</form>
<dialog
  class="confirm-dialog"
  bind:this={dialog!}
  onclose={() => {
    removeProvider = '';
  }}
>
  <h2>{t('Remove saved key?')}</h2>
  <p class="muted">{t('The deployment environment may still provide a key.')}</p>
  <div class="dialog-actions">
    <button class="button" type="button" disabled={busy} onclick={() => dialog.close()}
      >{t('Cancel')}</button
    ><button class="button danger" type="button" disabled={busy} onclick={removeKey}
      >{t('Remove')}</button
    >
  </div>
</dialog>
