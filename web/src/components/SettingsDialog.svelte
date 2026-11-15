<script lang="ts">
  import { onDestroy } from 'svelte';
  import { api, errorMessage, isAborted, sessionApi } from '../lib/api';
  import { appState, notify, t } from '../lib/appState.svelte';
  import type { Settings, SetupStatus } from '../lib/types';

  let dialog: HTMLDialogElement;
  let form: HTMLFormElement;
  let message = $state('');
  let saving = $state(false);
  let closeTimer: ReturnType<typeof setTimeout> | undefined;

  const providers = $derived(Object.entries(appState.settings?.providers || {}));
  const periods: [string, string][] = [
    ['Daily', 'daily_'],
    ['Weekly', 'weekly_'],
    ['Monthly', 'monthly_'],
  ];
  const scopes: [string, string, number][] = [
    ['user', 'Regular user jobs', 100000],
    ['admin', 'Administrator jobs', 100000],
    ['panel', 'Panel-wide jobs', 1000000],
  ];

  function ready(provider: string): boolean {
    return provider.startsWith('captcha-')
      ? Boolean(appState.settings?.captcha_configured?.[provider.slice('captcha-'.length)])
      : Boolean(appState.settings?.configured?.[provider]);
  }
  function clearSecrets() {
    form?.querySelectorAll<HTMLInputElement>('input[type="password"]').forEach((input) => {
      input.value = '';
    });
  }
  // Show the saved configuration; typed values are never kept as defaults.
  function populate() {
    const settings = appState.settings;
    if (!settings || !form) return;
    for (const [key, value] of Object.entries(settings)) {
      const field = form.elements.namedItem(key);
      if (
        (field instanceof HTMLInputElement || field instanceof HTMLSelectElement) &&
        field.type !== 'password' &&
        !key.endsWith('_api_key') &&
        (typeof value === 'string' || typeof value === 'number')
      ) {
        field.value = String(value);
      }
    }
  }
  export function open() {
    if (appState.user?.role !== 'admin' || !appState.settings) return;
    clearTimeout(closeTimer);
    form.reset();
    clearSecrets();
    populate();
    message = '';
    dialog.showModal();
  }
  function close() {
    clearTimeout(closeTimer);
    if (dialog?.open) dialog.close();
  }
  $effect(() => {
    if (appState.view !== 'admin') close();
  });
  onDestroy(() => {
    clearTimeout(closeTimer);
    clearSecrets();
    if (dialog?.open) dialog.close();
  });
  async function refreshAuthConfiguration() {
    appState.auth = await api<SetupStatus>('/api/auth/setup-status');
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    const payload = Object.fromEntries(new FormData(form).entries());
    saving = true;
    try {
      const saved = await sessionApi<Settings>('/api/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      clearSecrets();
      appState.settings = saved;
      populate();
      message = t('Saved');
      await refreshAuthConfiguration();
      closeTimer = setTimeout(() => {
        message = '';
        close();
      }, 600);
    } catch (cause) {
      if (!isAborted(cause)) message = errorMessage(cause);
    } finally {
      saving = false;
    }
  }
  async function removeKey(provider: string, label: string) {
    if (!window.confirm(t('Remove the saved secret for {provider}?', { provider: label }))) return;
    try {
      appState.settings = await sessionApi<Settings>(
        `/api/settings/keys/${encodeURIComponent(provider)}`,
        { method: 'DELETE' },
      );
      populate();
      await refreshAuthConfiguration();
      notify(t('Secret removed'));
    } catch (cause) {
      if (!isAborted(cause)) notify(errorMessage(cause));
    }
  }
</script>

{#snippet keyRow(name: string, provider: string, label: string)}
  <div class="key-row">
    <input
      type="password"
      {name}
      autocomplete="new-password"
      placeholder={t('Leave blank to keep current key')}
    /><span class="key-state" class:ready={ready(provider)} data-provider={provider}
      >{ready(provider) ? t('Configured') : t('Not set')}</span
    ><button
      class="clear-key"
      data-provider={provider}
      type="button"
      hidden={!ready(provider)}
      onclick={() => removeKey(provider, label)}>{t('Remove')}</button
    >
  </div>
{/snippet}

<dialog id="settingsDialog" bind:this={dialog} onclose={clearSecrets}>
  <form id="settingsForm" bind:this={form} onsubmit={save}>
    <div class="dialog-heading">
      <div>
        <span class="eyebrow">{t('Administration')}</span>
        <h2>{t('Panel settings')}</h2>
      </div>
      <button
        class="icon-button"
        id="closeSettings"
        type="button"
        aria-label={t('Close')}
        onclick={close}>×</button
      >
    </div>
    <p class="settings-intro">
      {t(
        'Provider and CAPTCHA secrets are write-only, encrypted before database storage, and available only to administrators.',
      )}
    </p>
    <div class="settings-group">
      <h3>{t('Providers')}</h3>
      <label
        >{t('Default provider')}<select name="default_provider" id="defaultProvider">
          {#each providers as [value, label] (value)}<option {value}>{label}</option>{/each}
        </select></label
      >
      <label
        >{t('Anthropic API key')}{@render keyRow(
          'anthropic_api_key',
          'anthropic',
          t('Anthropic API key'),
        )}</label
      >
      <label
        >{t('Anthropic model')}<input
          name="anthropic_model"
          placeholder="claude-sonnet-4-6"
        /></label
      >
      <label
        >{t('OpenAI-compatible API key')}{@render keyRow(
          'openai_api_key',
          'openai',
          t('OpenAI-compatible API key'),
        )}</label
      >
      <label
        >{t('OpenAI-compatible model')}<input name="openai_model" placeholder="gpt-5-mini" /></label
      >
      <label
        >{t('OpenAI-compatible base URL')}<input
          name="openai_base_url"
          placeholder="https://api.openai.com/v1"
        /></label
      >
      <label
        >{t('DeepL API key')}{@render keyRow('deepl_api_key', 'deepl', t('DeepL API key'))}</label
      >
      <label
        >{t('Google Cloud Translation API key')}{@render keyRow(
          'google_api_key',
          'google',
          t('Google Cloud Translation API key'),
        )}</label
      >
    </div>
    <details>
      <summary>{t('Registration and CAPTCHA')}</summary>
      <p class="settings-intro">
        {t(
          'CAPTCHA tokens are verified by the server. Choose which actions require a fresh challenge; a broken active configuration fails closed.',
        )}
      </p>
      <div class="settings-group compact-grid">
        <label
          >{t('Allow new user sign-up')}<select name="registration_enabled"
            ><option value="1">{t('Enabled')}</option><option value="0">{t('Disabled')}</option
            ></select
          ></label
        >
        <label
          >{t('CAPTCHA provider')}<select name="captcha_provider"
            ><option value="none">{t('Disabled')}</option><option value="turnstile"
              >Cloudflare Turnstile</option
            ><option value="recaptcha">Google reCAPTCHA v2</option><option value="hcaptcha"
              >hCaptcha</option
            ></select
          ></label
        >
        <label class="wide"
          >{t('Expected hostname')}<input
            name="captcha_hostname"
            placeholder="translate.example.com"
          /><small>{t('Required whenever a CAPTCHA provider is selected')}</small></label
        >
        <label
          >{t('Protect login')}<select name="captcha_on_login"
            ><option value="1">{t('Enabled')}</option><option value="0">{t('Disabled')}</option
            ></select
          ></label
        >
        <label
          >{t('Protect registration')}<select name="captcha_on_register"
            ><option value="1">{t('Enabled')}</option><option value="0">{t('Disabled')}</option
            ></select
          ></label
        >
        <label
          >{t('Protect uploads')}<select name="captcha_on_upload"
            ><option value="1">{t('Enabled')}</option><option value="0">{t('Disabled')}</option
            ></select
          ></label
        >
      </div>
      <div class="settings-group">
        <h3>Cloudflare Turnstile</h3>
        <label>{t('Site key')}<input name="turnstile_site_key" autocomplete="off" /></label>
        <label
          >{t('Secret key')}{@render keyRow(
            'turnstile_secret_key',
            'captcha-turnstile',
            'Cloudflare Turnstile',
          )}</label
        >
        <h3>Google reCAPTCHA v2</h3>
        <label>{t('Site key')}<input name="recaptcha_site_key" autocomplete="off" /></label>
        <label
          >{t('Secret key')}{@render keyRow(
            'recaptcha_secret_key',
            'captcha-recaptcha',
            'Google reCAPTCHA v2',
          )}</label
        >
        <h3>hCaptcha</h3>
        <label>{t('Site key')}<input name="hcaptcha_site_key" autocomplete="off" /></label>
        <label
          >{t('Secret key')}{@render keyRow(
            'hcaptcha_secret_key',
            'captcha-hcaptcha',
            'hCaptcha',
          )}</label
        >
      </div>
    </details>
    <details>
      <summary>{t('Translation defaults')}</summary>
      <div class="settings-group compact-grid">
        <label>{t('Batch size')}<input type="number" name="batch_size" min="1" max="100" /></label>
        <label
          >{t('Parallel requests')}<input type="number" name="workers" min="1" max="16" /></label
        >
        <label
          >{t('Requests / minute')}<input
            type="number"
            name="rpm"
            min="0"
            max="10000"
            step="any"
          /><small>{t('0 means provider-managed')}</small></label
        >
        <label
          >{t('Line width')}<input type="number" name="width" min="4" max="80" step="any" /></label
        >
        <label>{t('Maximum lines')}<input type="number" name="max_lines" min="1" max="5" /></label>
      </div>
    </details>
    <details>
      <summary>{t('Submission rate limits')}</summary>
      <p class="settings-intro">
        {t(
          'Each uploaded subtitle consumes one job. Account limits apply separately to each account; the panel limit covers all accounts. Use 0 for unlimited.',
        )}
      </p>
      <div class="settings-group compact-grid">
        <label
          >{t('Window (minutes)')}<input
            type="number"
            name="rate_limit_window_minutes"
            min="1"
            max="10080"
            step="1"
          /></label
        >
        {#each scopes as [scope, label, max] (scope)}<label
            >{t(label)}<input
              type="number"
              name={`${scope}_job_limit`}
              min="0"
              {max}
              step="1"
            /></label
          >{/each}
      </div>
      <p class="settings-intro">
        {t(
          'Calendar limits apply alongside the submission window. Resets use UTC: midnight daily, Monday weekly, and the first day of each month. Saving settings preserves calendar usage.',
        )}
      </p>
      {#each periods as [label, prefix] (prefix)}
        <h3>{t(label)}</h3>
        <div class="settings-group compact-grid">
          {#each scopes as [scope, scopeLabel, max] (scope)}<label
              >{t(scopeLabel)}<input
                type="number"
                name={`${scope}_${prefix}job_limit`}
                min="0"
                {max}
                step="1"
              /></label
            >{/each}
        </div>
      {/each}
    </details>
    <div class="dialog-actions">
      <span id="settingsMessage">{message}</span><button
        class="primary"
        type="submit"
        disabled={saving}>{t('Save settings')}</button
      >
    </div>
  </form>
</dialog>
