<script lang="ts">
  import { untrack } from 'svelte';
  import Captcha from './Captcha.svelte';
  import { api, errorMessage, isAborted } from '../lib/api';
  import { readBoot } from '../lib/boot';
  import { appState, languageName, notify, providerLabel, t } from '../lib/appState.svelte';
  import { loadJobs } from '../lib/workspace.svelte';

  const boot = readBoot();
  const codes = $derived(
    boot.languages.length ? boot.languages : Object.keys(appState.catalog.languages),
  );
  let files = $state.raw<File[]>([]);
  let provider = $state('');
  let source = $state('');
  let targets = $state<string[]>([]);
  let model = $state('');
  let busy = $state(false);
  let dragging = $state(false);
  let fileInput: HTMLInputElement;
  let captchaToken = $state('');
  let captchaGeneration = $state(0);
  const configured = $derived(Boolean(appState.settings?.configured?.[provider]));
  const captchaRequired = $derived(
    appState.auth?.captcha.provider !== 'none' &&
      Boolean(appState.auth?.captcha.protected_actions.includes('upload')),
  );

  // Saved settings are the defaults; they are applied again whenever settings are reloaded.
  $effect(() => {
    const settings = appState.settings;
    if (!settings) return;
    untrack(() => {
      provider = String(settings.default_provider ?? '');
      source = String(settings.source_language ?? '');
      targets = String(settings.target_languages ?? '')
        .split(',')
        .filter(Boolean);
    });
  });

  function syncFiles() {
    files = [...(fileInput.files || [])];
  }
  function drop(event: DragEvent) {
    event.preventDefault();
    dragging = false;
    if (busy || !event.dataTransfer?.files.length) return;
    fileInput.files = event.dataTransfer.files;
    syncFiles();
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!files.length) return notify(t('Choose at least one subtitle file'));
    if (!targets.length) return notify(t('Choose at least one target language'));
    if (!appState.settings?.configured?.[provider]) {
      return notify(t('This provider is not configured'));
    }
    if (captchaRequired && !captchaToken) return notify(t('Complete the CAPTCHA challenge'));
    const generation = appState.session;
    busy = true;
    try {
      const data = new FormData();
      files.forEach((file) => data.append('files', file));
      data.set('provider', provider);
      data.set('source_language', source);
      data.set('model', model);
      // The original form had no encoding control; the API defaults to UTF-8.
      data.set('encoding', 'utf-8');
      data.set('target_languages', targets.join(','));
      if (captchaToken) data.set('captcha_token', captchaToken);
      await api('/api/jobs', { method: 'POST', body: data });
      if (generation !== appState.session) return;
      const count = files.length;
      model = '';
      fileInput.value = '';
      syncFiles();
      notify(count === 1 ? t('Translation queued') : t('{count} translations queued', { count }));
      await loadJobs();
    } catch (cause) {
      if (generation === appState.session && !isAborted(cause)) notify(errorMessage(cause));
    } finally {
      if (generation === appState.session) {
        busy = false;
        captchaGeneration++;
      }
    }
  }
</script>

<div class="page-intro">
  <span class="eyebrow">SRT · WEBVTT · ASS · SSA</span>
  <h2>{t('Translate subtitles without losing the performance.')}</h2>
  <p>
    {t(
      'Upload one file or a whole season. Dialogue, timing, positioning, styles, and inline tags stay where they belong.',
    )}
  </p>
</div>
<section class="panel translator-panel">
  <form id="translateForm" onsubmit={submit}>
    <label
      class="dropzone"
      class:dragging
      id="dropzone"
      for="fileInput"
      ondragenter={(event) => {
        event.preventDefault();
        dragging = true;
      }}
      ondragover={(event) => {
        event.preventDefault();
        dragging = true;
      }}
      ondragleave={(event) => {
        event.preventDefault();
        dragging = false;
      }}
      ondrop={drop}
    >
      <input
        id="fileInput"
        name="files"
        type="file"
        multiple
        accept=".srt,.vtt,.ass,.ssa"
        bind:this={fileInput}
        onchange={syncFiles}
      />
      <span class="drop-icon">↑</span>
      <strong>{t('Drop subtitle files here')}</strong>
      <span
        >{t('or click to browse · up to {max_upload_mb} MB per request', {
          max_upload_mb: boot.maxUploadMB,
        })}</span
      >
      <div class="file-list" id="fileList">
        {#each files as file, index (`${index}:${file.name}`)}<span class="file-pill"
            >{file.name}</span
          >{/each}
      </div>
    </label>
    <div class="form-grid">
      <label
        >{t('Source language')}<input
          name="source_language"
          id="sourceLanguage"
          bind:value={source}
          placeholder={t('Auto or English')}
        /></label
      >
      <label
        >{t('Provider')}<select name="provider" id="provider" bind:value={provider}>
          {#each Object.entries(appState.settings?.providers || {}) as [value, label] (value)}
            <option {value}>{label}</option>
          {/each}
        </select></label
      >
      <fieldset class="wide field-group">
        <legend>{t('Target languages')}</legend>
        <div class="language-picker" id="languagePicker">
          {#each codes as code (code)}
            <label class="chip"
              ><input type="checkbox" value={code} bind:group={targets} /><span
                >{languageName(code)}</span
              ></label
            >
          {/each}
        </div>
      </fieldset>
      <label
        class="wide optional-model"
        id="modelField"
        style:display={provider === 'anthropic' || provider === 'openai' ? undefined : 'none'}
        >{t('Model override')} <span>({t('optional')})</span>
        <input name="model" bind:value={model} placeholder={t('Use the configured model')} />
      </label>
    </div>
    <Captcha
      id="uploadCaptcha"
      wide
      action="upload"
      onToken={(value) => {
        captchaToken = value;
      }}
      generation={captchaGeneration}
    />
    <div class="submit-row">
      <p id="providerState">
        {#if provider === 'echo'}{t('Offline test mode — no API calls')}
        {:else if configured}{t('{provider} is configured', {
            provider: providerLabel(provider),
          })}{:else}{t('Ask an administrator to configure this provider')}{/if}
      </p>
      <button class="primary" type="submit" id="submitButton" disabled={busy}
        >{t('Start translating')} <span>→</span></button
      >
    </div>
  </form>
</section>
