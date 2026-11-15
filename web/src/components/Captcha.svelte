<script lang="ts">
  import { appState, t } from '../lib/appState.svelte';
  import { errorMessage } from '../lib/api';

  let {
    action,
    onToken,
    generation = 0,
  }: {
    action: string;
    onToken: (token: string) => void;
    generation?: number;
  } = $props();
  let container = $state<HTMLDivElement>();
  let error = $state('');
  const urls = {
    turnstile: 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit',
    recaptcha: 'https://www.google.com/recaptcha/api.js?render=explicit',
    hcaptcha: 'https://js.hcaptcha.com/1/api.js?render=explicit&recaptchacompat=off',
  };
  interface CaptchaSdk {
    render: (container: HTMLElement, options: Record<string, unknown>) => string | number;
    remove?: (id: string | number) => void;
    reset: (id: string | number) => void;
  }
  function sdk(provider: string): CaptchaSdk | undefined {
    const globals = window as unknown as Record<string, CaptchaSdk>;
    return globals[
      { turnstile: 'turnstile', recaptcha: 'grecaptcha', hcaptcha: 'hcaptcha' }[provider] || ''
    ];
  }
  const required = $derived(
    appState.auth?.captcha.provider !== 'none' &&
      Boolean(appState.auth?.captcha.protected_actions.includes(action)),
  );

  $effect(() => {
    const config = appState.auth?.captcha;
    const theme = appState.scheme;
    const currentAction = action;
    void generation;
    onToken('');
    error = '';
    if (!required || !config || config.provider === 'none' || !container) return;
    const provider = config.provider;
    let alive = true;
    let id: string | number | undefined;
    let timer: ReturnType<typeof setTimeout>;
    const target = document.createElement('div');
    container.replaceChildren(target);
    if (!config.site_key) {
      error = t('CAPTCHA is temporarily unavailable');
      return;
    }
    const existing = [...document.scripts].find((script) => script.src === urls[provider]);
    if (!existing) {
      const script = document.createElement('script');
      script.src = urls[provider];
      script.async = true;
      script.defer = true;
      script.onerror = () => {
        if (alive) error = t('Could not load CAPTCHA');
        script.remove();
      };
      document.head.append(script);
    }
    let checks = 0;
    const render = () => {
      if (!alive) return;
      const api = sdk(provider);
      if (typeof api?.render !== 'function') {
        if (++checks > 150) {
          error = t('Could not load CAPTCHA');
          return;
        }
        timer = setTimeout(render, 100);
        return;
      }
      try {
        id = api.render(target, {
          sitekey: config.site_key,
          theme,
          ...(provider === 'turnstile' ? { action: currentAction } : {}),
          callback: (token: string) => {
            if (alive) onToken(token);
          },
          'expired-callback': () => {
            if (alive) onToken('');
          },
          'error-callback': () => {
            if (alive) {
              onToken('');
              error = t('CAPTCHA verification failed; please try again');
            }
          },
        });
      } catch (cause) {
        if (alive) error = errorMessage(cause);
      }
    };
    render();
    return () => {
      alive = false;
      clearTimeout(timer);
      if (id !== undefined) {
        try {
          const api = sdk(provider);
          if (api?.remove) api.remove(id);
          else api?.reset(id);
        } catch {
          /* The detached container no longer accepts callbacks. */
        }
      }
      target.remove();
    };
  });
</script>

{#if required}
  <div class="captcha-slot">
    <div bind:this={container!}></div>
    {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  </div>
{/if}
