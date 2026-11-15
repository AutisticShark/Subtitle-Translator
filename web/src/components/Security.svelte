<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import { errorMessage, isAborted, json, sessionApi } from '../lib/api';
  import { appState, notify, t } from '../lib/appState.svelte';
  import type { AuthResponse, Challenge, MfaStatus } from '../lib/types';

  let status = $state<MfaStatus | null>(null);
  let enrollment = $state<Challenge | null>(null);
  let management = $state<Challenge | null>(null);
  let recoveryCodes = $state<string[]>([]);
  let method = $state<'totp' | 'email'>('totp');
  let setupEmail = $state('');
  let setupPassword = $state('');
  let confirmCode = $state('');
  let managePassword = $state('');
  let manageCode = $state('');
  let message = $state('');
  let busy = $state(false);
  let managePasswordInput = $state<HTMLInputElement>();
  let alive = true;
  const generation = appState.session;
  function current() {
    return alive && generation === appState.session;
  }
  const emailMethod = $derived(method === 'email');

  function clearSecrets() {
    enrollment = null;
    management = null;
    recoveryCodes = [];
    setupEmail = '';
    setupPassword = '';
    confirmCode = '';
    managePassword = '';
    manageCode = '';
    method = 'totp';
    message = '';
  }
  async function loadMfa() {
    try {
      const data = await sessionApi<MfaStatus>('/api/auth/mfa');
      if (current()) status = data;
    } catch (cause) {
      if (current() && !isAborted(cause)) message = errorMessage(cause);
    }
  }
  onMount(() => {
    void loadMfa();
  });
  onDestroy(() => {
    alive = false;
    clearSecrets();
  });
  const statusText = $derived.by(() => {
    if (!status) return '';
    if (!status.enrollment_available) return t('Configure JWT_SECRET_KEY before saving secrets');
    return status.method
      ? t('MFA enabled: {method}. Recovery codes remaining: {count}.', {
          method: status.method === 'totp' ? t('Authenticator app') : status.email,
          count: status.recovery_remaining,
        })
      : t('MFA is not enabled');
  });

  async function startSetup(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    message = '';
    try {
      const payload: Record<string, string> = { method, password: setupPassword };
      if (emailMethod) payload.email = setupEmail;
      const data = await json<Challenge>('/api/auth/mfa/setup', payload);
      if (!current()) return;
      enrollment = data;
      setupPassword = '';
      setupEmail = '';
      method = 'totp';
      confirmCode = '';
      message = '';
    } catch (cause) {
      if (current()) message = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  async function changed(data: AuthResponse) {
    if (!current()) return;
    if (data.user) appState.user = data.user;
    clearSecrets();
    recoveryCodes = data.recovery_codes || [];
    await loadMfa();
  }
  async function confirm(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    message = '';
    try {
      const data = await json<AuthResponse>('/api/auth/mfa/confirm', {
        challenge_token: enrollment?.challenge_token,
        code: confirmCode,
      });
      await changed(data);
    } catch (cause) {
      if (current()) message = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  async function resend() {
    busy = true;
    try {
      const data = await json<Challenge>('/api/auth/mfa/resend', {
        challenge_token: enrollment?.challenge_token,
      });
      if (current()) {
        enrollment = data;
        message = data.warning || t('Verification email sent');
      }
    } catch (cause) {
      if (current()) message = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  async function sendManagementEmail() {
    if (!managePasswordInput?.reportValidity()) return;
    busy = true;
    try {
      const data = await json<Challenge>('/api/auth/mfa/email', { password: managePassword });
      if (current()) {
        management = data;
        message = data.warning || t('Verification email sent');
      }
    } catch (cause) {
      if (current()) message = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  async function manage(event: SubmitEvent) {
    event.preventDefault();
    const operation = (event.submitter as HTMLButtonElement | null)?.value;
    if (operation !== 'disable' && operation !== 'recovery') return;
    busy = true;
    message = '';
    try {
      const data = await json<AuthResponse>(`/api/auth/mfa/${operation}`, {
        password: managePassword,
        code: manageCode,
        challenge_token: management?.challenge_token,
      });
      await changed(data);
      if (current()) notify(t('Account security updated'));
    } catch (cause) {
      if (current()) message = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  function cancelSetup() {
    clearSecrets();
    void loadMfa();
  }
  function downloadRecovery() {
    const content = `Subtitle Translator — ${appState.user?.username}\n\n${recoveryCodes.join('\n')}\n`;
    const url = URL.createObjectURL(new Blob([content], { type: 'text/plain;charset=utf-8' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = 'subtitle-translator-recovery-codes.txt';
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
</script>

<div class="page-intro">
  <h2>{t('Multi-factor authentication')}</h2>
  <p>
    {t(
      'Protect your account with an authenticator app or email verification codes. Keep recovery codes somewhere safe.',
    )}
  </p>
</div>
<section class="panel dashboard-card security-panel">
  <p id="mfaStatus" role="status">
    {statusText}{#if status && !status.email_available && !status.method}<br />{t(
        'Email verification is unavailable; ask an administrator to configure email delivery',
      )}{/if}
  </p>
  <p id="mfaSecurityError" role="alert">{message}</p>
  {#if status && !status.method && status.enrollment_available && !enrollment}
    <form id="mfaSetupForm" onsubmit={startSetup}>
      <label
        >{t('MFA method')}<select name="method" id="mfaMethod" bind:value={method}
          ><option value="totp">{t('Authenticator app (recommended)')}</option><option
            value="email"
            disabled={!status.email_available}>{t('Email verification code')}</option
          ></select
        ></label
      >
      <p>
        {t(
          'Google Authenticator, Microsoft Authenticator, and other TOTP apps are supported. Email security depends on your mailbox security.',
        )}
      </p>
      {#if emailMethod}<label id="mfaEmailField"
          >{t('Email address')}<input
            name="email"
            type="email"
            bind:value={setupEmail}
            required
            maxlength="254"
            autocomplete="email"
          /></label
        >{/if}
      <label
        >{t('Current password')}<input
          name="password"
          type="password"
          bind:value={setupPassword}
          autocomplete="current-password"
          required
          maxlength="256"
        /></label
      >
      <button class="primary" type="submit" disabled={busy}>{t('Start MFA setup')}</button>
    </form>
  {/if}
  {#if enrollment}
    <form id="mfaConfirmForm" onsubmit={confirm}>
      {#if enrollment.method === 'totp'}
        <div id="mfaAuthenticator">
          <p>
            {t(
              'Scan this QR code in your authenticator app, or enter the setup key manually. Then enter a new six-digit code.',
            )}
          </p>
          {#if enrollment.qr_code}<img
              id="mfaQr"
              class="mfa-qr"
              src={enrollment.qr_code}
              alt={t('Authenticator setup QR code')}
            />{/if}
          <label
            >{t('Setup key')}<input
              id="mfaSecret"
              value={enrollment.secret || ''}
              readonly
              autocomplete="off"
            /></label
          >
        </div>
      {/if}
      <p id="mfaEnrollmentNotice" role="status">
        {enrollment.warning ||
          (enrollment.method === 'email'
            ? t('Enter the six-digit email code within 5 minutes to enable MFA.')
            : t('Setup expires in 10 minutes. MFA starts only after you verify the code.'))}
      </p>
      <label
        >{t('Verification code')}<input
          name="code"
          bind:value={confirmCode}
          inputmode="numeric"
          autocomplete="one-time-code"
          pattern={'[0-9]{6}'}
          required
          maxlength="6"
        /></label
      >
      <button class="primary" type="submit" disabled={busy}>{t('Verify and enable MFA')}</button>
      {#if enrollment.method === 'email'}<button
          class="ghost"
          id="mfaSetupResend"
          type="button"
          disabled={busy}
          onclick={resend}>{t('Resend email code')}</button
        >{/if}
      <button class="text-button" id="mfaSetupCancel" type="button" onclick={cancelSetup}
        >{t('Cancel setup')}</button
      >
    </form>
  {/if}
  {#if status?.method}
    <form id="mfaManageForm" onsubmit={manage}>
      <p>
        {t(
          'To disable MFA or replace recovery codes, enter your password and an unused verification or recovery code. Other sessions will be signed out.',
        )}
      </p>
      <label
        >{t('Current password')}<input
          name="password"
          type="password"
          bind:this={managePasswordInput}
          bind:value={managePassword}
          autocomplete="current-password"
          required
          maxlength="256"
        /></label
      >
      {#if status.method === 'email'}<button
          class="ghost"
          id="mfaManageEmail"
          type="button"
          disabled={busy}
          onclick={sendManagementEmail}>{t('Send email code')}</button
        >{/if}
      <label
        >{t('Verification or recovery code')}<input
          name="code"
          bind:value={manageCode}
          autocomplete="one-time-code"
          required
          maxlength="64"
          spellcheck="false"
        /></label
      >
      <button class="ghost" type="submit" value="recovery" disabled={busy}
        >{t('Replace recovery codes')}</button
      >
      <button class="danger" type="submit" value="disable" disabled={busy}
        >{t('Disable MFA')}</button
      >
    </form>
  {/if}
  {#if recoveryCodes.length}
    <div id="mfaRecovery">
      <h3>{t('Save your recovery codes')}</h3>
      <p>
        {t(
          'These codes are shown only once. Each code can be used once instead of your second factor. Your password is still required. Store them outside this browser.',
        )}
      </p>
      <pre id="mfaRecoveryCodes">{recoveryCodes.join('\n')}</pre>
      <button class="primary" id="mfaDownloadRecovery" type="button" onclick={downloadRecovery}
        >{t('Download recovery codes')}</button
      >
      <button
        class="ghost"
        id="mfaSavedRecovery"
        type="button"
        onclick={() => {
          recoveryCodes = [];
        }}>{t('I have saved my codes')}</button
      >
    </div>
  {/if}
</section>
