<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import Icon from './Icon.svelte';
  import { errorMessage, isAborted, json, sessionApi } from '../lib/api';
  import { notify, appState, t } from '../lib/appState.svelte';
  import type { AuthResponse, Challenge, MfaStatus } from '../lib/types';

  let status = $state<MfaStatus | null>(null);
  let challenge = $state<Challenge | null>(null);
  let managementChallenge = $state<Challenge | null>(null);
  let recoveryCodes = $state<string[]>([]);
  let method = $state<'totp' | 'email'>('totp');
  let password = $state('');
  let email = $state('');
  let code = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let alive = true;
  const generation = appState.session;
  function current() {
    return alive && generation === appState.session;
  }
  async function loadStatus() {
    try {
      const data = await sessionApi<MfaStatus>('/api/auth/mfa');
      if (current()) status = data;
    } catch (cause) {
      if (current() && !isAborted(cause)) error = errorMessage(cause);
    }
  }
  onMount(() => {
    void loadStatus();
  });
  onDestroy(() => {
    alive = false;
    password = '';
    code = '';
    challenge = null;
    managementChallenge = null;
    recoveryCodes = [];
  });
  function resetSecrets() {
    password = '';
    code = '';
    challenge = null;
    managementChallenge = null;
  }
  async function setup(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = '';
    notice = '';
    try {
      const data = await json<Challenge>('/api/auth/mfa/setup', { method, email, password });
      if (!current()) return;
      challenge = data;
      code = '';
      notice = data.warning || '';
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      password = '';
      if (current()) busy = false;
    }
  }
  async function changed(data: AuthResponse) {
    if (!current()) return;
    if (data.user) appState.user = data.user;
    resetSecrets();
    recoveryCodes = data.recovery_codes || [];
    await loadStatus();
  }
  async function confirm(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = '';
    notice = '';
    try {
      const data = await json<AuthResponse>('/api/auth/mfa/confirm', {
        challenge_token: challenge?.challenge_token,
        code,
      });
      await changed(data);
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      code = '';
      if (current()) busy = false;
    }
  }
  async function resend() {
    busy = true;
    error = '';
    try {
      const data = await json<Challenge>('/api/auth/mfa/resend', {
        challenge_token: challenge?.challenge_token,
      });
      if (current()) {
        challenge = data;
        notice = data.warning || t('Verification email sent');
      }
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  async function sendManagementEmail() {
    if (!password) {
      error = t('Enter your current password');
      return;
    }
    busy = true;
    error = '';
    try {
      const data = await json<Challenge>('/api/auth/mfa/email', { password });
      if (current()) {
        managementChallenge = data;
        notice = data.warning || t('Verification email sent');
      }
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  async function manage(event: SubmitEvent) {
    event.preventDefault();
    const operation = (event.submitter as HTMLButtonElement | null)?.value;
    if (operation !== 'disable' && operation !== 'recovery') return;
    busy = true;
    error = '';
    notice = '';
    try {
      const data = await json<AuthResponse>(`/api/auth/mfa/${operation}`, {
        password,
        code,
        challenge_token: managementChallenge?.challenge_token,
      });
      await changed(data);
      if (current()) notify(t('Account security updated'));
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      password = '';
      code = '';
      if (current()) busy = false;
    }
  }
  function downloadRecovery() {
    const url = URL.createObjectURL(
      new Blob(
        [`Subtitle Translator — ${appState.user?.username}\n\n${recoveryCodes.join('\n')}\n`],
        { type: 'text/plain;charset=utf-8' },
      ),
    );
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = 'subtitle-translator-recovery-codes.txt';
    anchor.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
</script>

<header class="page-header"><h1>{t('Account')}</h1></header>
<div class="account-summary">
  <span class="avatar large">{appState.user?.username.slice(0, 1).toUpperCase()}</span>
  <div>
    <h2>{appState.user?.username}</h2>
    <span class="muted">{t(appState.user?.role === 'admin' ? 'Administrator' : 'User')}</span>
  </div>
</div>
<section class="account-section">
  <div class="section-heading">
    <h2>{t('Two-step verification')}</h2>
    {#if status}<span class="security-status"
        ><span class="status-dot" class:completed={Boolean(status.method)}></span>{status.method
          ? t('Enabled')
          : t('Not enabled')}</span
      >{/if}
  </div>
  {#if error}<p class="inline-error" role="alert">{error}</p>{/if}
  {#if notice}<p class="inline-notice" role="status">{notice}</p>{/if}
  {#if recoveryCodes.length}
    <div class="recovery-panel">
      <h3>{t('Save your recovery codes')}</h3>
      <p class="muted">
        {t(
          'These codes are shown only once. Store them outside this browser. Each code can be used once instead of your second factor.',
        )}
      </p>
      <ol class="recovery-codes">
        {#each recoveryCodes as recovery}<li>{recovery}</li>{/each}
      </ol>
      <div class="form-actions">
        <button class="primary" type="button" onclick={downloadRecovery}
          ><Icon name="download" size={16} />{t('Download recovery codes')}</button
        ><button
          class="button"
          type="button"
          onclick={() => {
            recoveryCodes = [];
          }}>{t('I have saved my codes')}</button
        >
      </div>
    </div>
  {:else if !status}
    <div class="empty-state"><span class="loading-line"></span></div>
  {:else if challenge}
    <form class="security-form stack" onsubmit={confirm}>
      {#if challenge.method === 'totp'}
        <p class="muted">
          {t('Scan the QR code with your authenticator app, then enter a six-digit code.')}
        </p>
        {#if challenge.qr_code}<img
            class="mfa-qr"
            src={challenge.qr_code}
            alt={t('Authenticator setup QR code')}
            width="192"
            height="192"
          />{/if}
        <label
          >{t('Setup key')}<input
            class="setup-key"
            value={challenge.secret || ''}
            readonly
            autocomplete="off"
          /></label
        >
      {:else}<p class="muted">
          {t('Enter the six-digit email code within 5 minutes to enable MFA.')}
        </p>{/if}
      <label
        >{t('Verification code')}<input
          bind:value={code}
          inputmode="numeric"
          autocomplete="one-time-code"
          pattern={'[0-9]{6}'}
          required
          maxlength="6"
        /></label
      >
      <div class="form-actions">
        <button class="primary" type="submit" disabled={busy}
          >{busy ? t('Verifying…') : t('Enable two-step verification')}</button
        >{#if challenge.method === 'email'}<button
            class="button"
            type="button"
            disabled={busy}
            onclick={resend}>{t('Resend email code')}</button
          >{/if}<button
          class="text-button"
          type="button"
          disabled={busy}
          onclick={() => {
            resetSecrets();
            notice = '';
            error = '';
          }}>{t('Cancel setup')}</button
        >
      </div>
    </form>
  {:else if !status.method}
    {#if !status.enrollment_available}<p class="muted">
        {t('Configure JWT_SECRET_KEY before saving secrets')}
      </p>
    {:else}<form class="security-form stack" onsubmit={setup}>
        <label
          >{t('Verification method')}<select bind:value={method}
            ><option value="totp">{t('Authenticator app')}</option><option
              value="email"
              disabled={!status.email_available}>{t('Email verification code')}</option
            ></select
          ></label
        >
        {#if method === 'email'}<label
            >{t('Email address')}<input
              bind:value={email}
              type="email"
              required
              maxlength="254"
              autocomplete="email"
            /></label
          >{/if}
        <label
          >{t('Current password')}<input
            bind:value={password}
            type="password"
            autocomplete="current-password"
            required
            maxlength="256"
          /></label
        >
        <div class="form-actions">
          <button type="submit" class="primary" disabled={busy}
            >{busy ? t('Please wait…') : t('Set up two-step verification')}</button
          >
        </div>
      </form>{/if}
  {:else}
    <div class="factor-summary">
      <Icon name="shield" size={20} /><span
        >{status.method === 'totp' ? t('Authenticator app') : status.email}</span
      ><span class="muted"
        >{t('{count} recovery codes remaining', { count: status.recovery_remaining })}</span
      >
    </div>
    <form class="security-form stack" onsubmit={manage}>
      <label
        >{t('Current password')}<input
          bind:value={password}
          type="password"
          autocomplete="current-password"
          required
          maxlength="256"
        /></label
      >
      {#if status.method === 'email'}<button
          class="button align-start"
          type="button"
          disabled={busy}
          onclick={sendManagementEmail}>{t('Send email code')}</button
        >{/if}
      <label
        >{t('Verification or recovery code')}<input
          bind:value={code}
          autocomplete="one-time-code"
          required
          maxlength="64"
          spellcheck="false"
        /></label
      >
      <div class="form-actions">
        <button class="button" type="submit" value="recovery" disabled={busy}
          >{t('Replace recovery codes')}</button
        ><button class="button quiet danger-text" type="submit" value="disable" disabled={busy}
          >{t('Disable two-step verification')}</button
        >
      </div>
    </form>
  {/if}
</section>
