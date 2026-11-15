<script lang="ts">
  import { onDestroy } from 'svelte';
  import Brand from './Brand.svelte';
  import Captcha from './Captcha.svelte';
  import { errorMessage, json } from '../lib/api';
  import { appState, t } from '../lib/appState.svelte';
  import type { AuthResponse, Challenge, User } from '../lib/types';

  let { onSignedIn }: { onSignedIn: (user: User) => Promise<void> } = $props();
  let mode = $state<'login' | 'register'>('login');
  let username = $state('');
  let password = $state('');
  let confirmPassword = $state('');
  let code = $state('');
  let challenge = $state<Challenge | null>(null);
  let busy = $state(false);
  let error = $state('');
  let mfaError = $state('');
  let captchaToken = $state('');
  let captchaGeneration = $state(0);
  const setup = $derived(!appState.auth?.configured);
  const registering = $derived(!setup && mode === 'register');
  const captchaRequired = $derived(
    !setup &&
      appState.auth?.captcha.provider !== 'none' &&
      Boolean(appState.auth?.captcha.protected_actions.includes(mode)),
  );

  onDestroy(() => {
    password = '';
    confirmPassword = '';
    code = '';
    challenge = null;
  });
  function switchMode() {
    username = '';
    password = '';
    confirmPassword = '';
    mode = mode === 'login' ? 'register' : 'login';
    error = '';
    captchaGeneration++;
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    try {
      if (registering && password !== confirmPassword) throw new Error(t('Passwords do not match'));
      if (captchaRequired && !captchaToken) throw new Error(t('Complete the CAPTCHA challenge'));
      busy = true;
      const payload: Record<string, string> = { username, password };
      if (registering) payload.confirm_password = confirmPassword;
      payload.captcha_token = captchaToken;
      const result = await json<AuthResponse>(
        `/api/auth/${setup ? 'setup' : registering ? 'register' : 'login'}`,
        payload,
      );
      username = '';
      password = '';
      confirmPassword = '';
      if (result.mfa_required && result.challenge_token && result.method) {
        challenge = result as Challenge;
        mfaError = result.warning || '';
      } else if (result.user) await onSignedIn(result.user);
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
      captchaGeneration++;
    }
  }
  async function verify(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    mfaError = '';
    try {
      const data = await json<AuthResponse>('/api/auth/mfa/verify', {
        challenge_token: challenge?.challenge_token,
        code,
      });
      challenge = null;
      code = '';
      if (data.user) await onSignedIn(data.user);
    } catch (cause) {
      mfaError = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
  async function resend() {
    busy = true;
    try {
      challenge = await json<Challenge>('/api/auth/mfa/resend', {
        challenge_token: challenge?.challenge_token,
      });
      mfaError = challenge.warning || t('Verification email sent');
    } catch (cause) {
      mfaError = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
  function back() {
    challenge = null;
    code = '';
    mfaError = '';
    error = '';
    captchaGeneration++;
  }
</script>

<main class="auth-view" id="authView">
  <section class="panel auth-panel">
    <Brand />
    <div class="auth-copy">
      <span class="eyebrow" id="authEyebrow"
        >{setup ? t('Initial setup') : t('Authentication')}</span
      >
      <h1 id="authTitle">
        {challenge
          ? t('Multi-factor authentication')
          : setup
            ? t('Create the first administrator')
            : registering
              ? t('Create your account')
              : t('Sign in')}
      </h1>
      <p id="authDescription">
        {setup
          ? t('This one-time account will manage users, provider keys, and all jobs.')
          : registering
            ? t('Choose a username and a strong password to join this workspace.')
            : t('Use your Subtitle Translator account.')}
      </p>
    </div>
    {#if challenge}
      <form id="mfaLoginForm" onsubmit={verify}>
        <p id="mfaLoginDescription">
          {challenge.method === 'email'
            ? t('Enter the code sent to your email, or a recovery code.')
            : t('Enter a new code from your authenticator app, or a recovery code.')}
        </p>
        <label
          >{t('Verification or recovery code')}<input
            name="code"
            bind:value={code}
            autocomplete="one-time-code"
            required
            maxlength="64"
            spellcheck="false"
          /></label
        >
        <p id="mfaLoginError" role="alert">{mfaError}</p>
        <button class="primary" type="submit" disabled={busy}>{t('Verify and sign in')}</button>
        {#if challenge.method === 'email'}<button
            class="ghost"
            id="mfaLoginResend"
            type="button"
            disabled={busy}
            onclick={resend}>{t('Resend email code')}</button
          >{/if}
        <button class="text-button" id="mfaLoginBack" type="button" onclick={back}
          >{t('Back to sign in')}</button
        >
      </form>
    {:else}
      <form id="authForm" onsubmit={submit}>
        <label
          >{t('Username')}<input
            name="username"
            bind:value={username}
            autocomplete="username"
            required
            minlength="3"
            maxlength="64"
          /></label
        >
        <label
          >{t('Password')}<input
            name="password"
            type="password"
            bind:value={password}
            autocomplete={setup || registering ? 'new-password' : 'current-password'}
            required
            minlength="12"
            maxlength="256"
          /></label
        >
        {#if registering}<label id="confirmPasswordField"
            >{t('Confirm password')}<input
              name="confirm_password"
              type="password"
              bind:value={confirmPassword}
              autocomplete="new-password"
              required
              minlength="12"
              maxlength="256"
            /></label
          >{/if}
        {#if !setup}<Captcha
            id="authCaptcha"
            action={mode}
            onToken={(value) => {
              captchaToken = value;
            }}
            generation={captchaGeneration}
          />{/if}
        <div class="auth-error" id="authError" role="alert">{error}</div>
        <button class="primary" id="authSubmit" type="submit" disabled={busy}
          >{setup
            ? t('Create administrator')
            : registering
              ? t('Create account')
              : t('Sign in')}</button
        >
      </form>
      {#if !setup && appState.auth?.registration_enabled}
        <p class="auth-switch" id="authSwitch">
          <span id="authSwitchPrompt"
            >{registering ? t('Already have an account?') : t('Need an account?')}</span
          >
          <button
            class="text-button"
            id="authSwitchButton"
            type="button"
            onclick={switchMode}
            disabled={busy}>{registering ? t('Sign in') : t('Create one')}</button
          >
        </p>
      {/if}
    {/if}
  </section>
</main>
