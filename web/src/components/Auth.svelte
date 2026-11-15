<script lang="ts">
  import Brand from './Brand.svelte';
  import Captcha from './Captcha.svelte';
  import Icon from './Icon.svelte';
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
  let captchaToken = $state('');
  let captchaGeneration = $state(0);
  const setup = $derived(!appState.auth?.configured);
  const title = $derived(
    challenge
      ? t('Two-step verification')
      : setup
        ? t('Create administrator')
        : mode === 'register'
          ? t('Create account')
          : t('Sign in'),
  );
  const captchaRequired = $derived(
    !setup &&
      appState.auth?.captcha.provider !== 'none' &&
      Boolean(appState.auth?.captcha.protected_actions.includes(mode)),
  );

  function switchMode() {
    mode = mode === 'login' ? 'register' : 'login';
    password = '';
    confirmPassword = '';
    error = '';
    captchaGeneration++;
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    if ((setup || mode === 'register') && password !== confirmPassword) {
      error = t('Passwords do not match');
      return;
    }
    busy = true;
    try {
      const endpoint = setup ? 'setup' : mode;
      const result = await json<AuthResponse>(`/api/auth/${endpoint}`, {
        username,
        password,
        confirm_password: confirmPassword,
        captcha_token: captchaToken,
      });
      password = '';
      confirmPassword = '';
      if (result.mfa_required && result.challenge_token && result.method) {
        challenge = result as Challenge;
        error = result.warning || '';
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
    error = '';
    try {
      const data = await json<AuthResponse>('/api/auth/mfa/verify', {
        challenge_token: challenge?.challenge_token,
        code,
      });
      code = '';
      if (data.user) await onSignedIn(data.user);
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
  async function resend() {
    busy = true;
    error = '';
    try {
      challenge = await json<Challenge>('/api/auth/mfa/resend', {
        challenge_token: challenge?.challenge_token,
      });
      error = challenge.warning || t('Verification email sent');
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<main class="auth-page">
  <div class="auth-brand"><Brand /></div>
  <section class="auth-form" aria-labelledby="auth-title">
    <div class="auth-symbol"><Icon name={challenge ? 'shield' : 'lock'} size={22} /></div>
    <h1 id="auth-title">{title}</h1>
    {#if challenge}
      <p class="muted auth-hint">
        {challenge.method === 'email'
          ? t('Enter the code sent to your email, or a recovery code.')
          : t('Enter a code from your authenticator app, or a recovery code.')}
      </p>
      <form onsubmit={verify} class="stack">
        <label
          >{t('Verification or recovery code')}<input
            bind:value={code}
            autocomplete="one-time-code"
            required
            maxlength="64"
            spellcheck="false"
          /></label
        >
        {#if error}<p class="form-error" role="alert">{error}</p>{/if}
        <button class="primary full-width" type="submit" disabled={busy}
          >{busy ? t('Verifying…') : t('Verify and sign in')}<Icon name="arrow" size={16} /></button
        >
        {#if challenge.method === 'email'}<button
            type="button"
            class="button quiet"
            onclick={resend}
            disabled={busy}>{t('Resend email code')}</button
          >{/if}
        <button
          type="button"
          class="text-button"
          disabled={busy}
          onclick={() => {
            challenge = null;
            code = '';
            error = '';
          }}>{t('Back to sign in')}</button
        >
      </form>
    {:else}
      <form onsubmit={submit} class="stack">
        <label
          >{t('Username')}<input
            bind:value={username}
            autocomplete="username"
            required
            minlength="3"
            maxlength="64"
            autocapitalize="none"
            spellcheck="false"
          /></label
        >
        <label
          >{t('Password')}<input
            bind:value={password}
            type="password"
            autocomplete={setup || mode === 'register' ? 'new-password' : 'current-password'}
            required
            minlength={setup || mode === 'register' ? 12 : undefined}
            maxlength="256"
          /></label
        >
        {#if setup || mode === 'register'}<label
            >{t('Confirm password')}<input
              bind:value={confirmPassword}
              type="password"
              autocomplete="new-password"
              required
              minlength="12"
              maxlength="256"
            /></label
          >{/if}
        {#if !setup}<Captcha
            action={mode}
            onToken={(value) => {
              captchaToken = value;
            }}
            generation={captchaGeneration}
          />{/if}
        {#if error}<p class="form-error" role="alert">{error}</p>{/if}
        <button
          type="submit"
          class="primary full-width"
          disabled={busy || (captchaRequired && !captchaToken)}
        >
          {busy
            ? t('Please wait…')
            : setup
              ? t('Create administrator')
              : mode === 'register'
                ? t('Create account')
                : t('Sign in')}<Icon name="arrow" size={16} />
        </button>
      </form>
      {#if !setup && appState.auth?.registration_enabled}
        <div class="auth-switch">
          <span class="muted"
            >{mode === 'login' ? t('Need an account?') : t('Already have an account?')}</span
          ><button class="text-button" type="button" onclick={switchMode} disabled={busy}
            >{mode === 'login' ? t('Create one') : t('Sign in')}</button
          >
        </div>
      {/if}
    {/if}
  </section>
  <div class="auth-formats" aria-hidden="true">
    SRT<span>·</span>WebVTT<span>·</span>ASS<span>·</span>SSA
  </div>
</main>
