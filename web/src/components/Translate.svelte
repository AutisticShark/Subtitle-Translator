<script lang="ts">
  import { tick } from 'svelte';
  import Captcha from './Captcha.svelte';
  import Icon from './Icon.svelte';
  import JobList from './JobList.svelte';
  import { api, errorMessage, isAborted } from '../lib/api';
  import { languageName, navigate, notify, setting, appState, t } from '../lib/appState.svelte';

  let { onExpired }: { onExpired: () => Promise<void> } = $props();
  let files = $state<File[]>([]);
  let provider = $state(setting('default_provider', 'anthropic'));
  let source = $state(setting('source_language', 'English'));
  let targets = $state<string[]>(
    setting('target_languages', 'zh-TW,zh-CN').split(',').filter(Boolean),
  );
  let model = $state('');
  let encoding = $state('utf-8');
  let busy = $state(false);
  let error = $state('');
  let dragging = $state(false);
  let dragDepth = 0;
  let fileInput: HTMLInputElement;
  let captchaToken = $state('');
  let captchaGeneration = $state(0);
  let languageMenu: HTMLDetailsElement;
  const ready = $derived(Boolean(appState.settings?.configured[provider]));
  const captchaRequired = $derived(
    appState.auth?.captcha.provider !== 'none' &&
      Boolean(appState.auth?.captcha.protected_actions.includes('upload')),
  );
  const totalSize = $derived(files.reduce((sum, file) => sum + file.size, 0));
  const allLanguages = $derived(Object.entries(appState.catalog.languages));

  function formatSize(bytes: number): string {
    return bytes < 1024 * 1024
      ? `${Math.max(1, Math.round(bytes / 1024))} KB`
      : `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  }
  function addFiles(incoming: FileList | File[] | null) {
    if (!incoming) return;
    error = '';
    const added: File[] = [];
    for (const file of [...incoming]) {
      if (!/\.(srt|vtt|ass|ssa)$/i.test(file.name)) {
        error = t('Unsupported subtitle format: {filename}', { filename: file.name });
        continue;
      }
      if (
        ![...files, ...added].some(
          (existing) =>
            existing.name === file.name &&
            existing.size === file.size &&
            existing.lastModified === file.lastModified,
        )
      )
        added.push(file);
    }
    files = [...files, ...added];
    if (fileInput) fileInput.value = '';
  }
  function drop(event: DragEvent) {
    event.preventDefault();
    dragDepth = 0;
    dragging = false;
    if (!busy) addFiles(event.dataTransfer?.files || null);
  }
  function toggleTarget(code: string) {
    targets = targets.includes(code)
      ? targets.filter((value) => value !== code)
      : [...targets, code];
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    if (!files.length) {
      error = t('Choose at least one subtitle file');
      return;
    }
    if (!targets.length) {
      error = t('Choose at least one target language');
      return;
    }
    if (!ready) {
      error = t('This provider is not configured');
      return;
    }
    busy = true;
    const generation = appState.session;
    try {
      const form = new FormData();
      files.forEach((file) => form.append('files', file));
      form.set('provider', provider);
      form.set('source_language', source);
      form.set('target_languages', targets.join(','));
      form.set('model', model);
      form.set('encoding', encoding);
      if (captchaToken) form.set('captcha_token', captchaToken);
      await api<{ jobs: string[] }>('/api/jobs', { method: 'POST', body: form });
      if (generation !== appState.session) return;
      const count = files.length;
      files = [];
      model = '';
      appState.jobsRevision++;
      notify(count === 1 ? t('Translation queued') : t('{count} translations queued', { count }));
      await tick();
      document.getElementById('recent-jobs')?.scrollIntoView({ block: 'nearest' });
    } catch (cause) {
      if (generation === appState.session && !isAborted(cause)) error = errorMessage(cause);
    } finally {
      if (generation === appState.session) {
        busy = false;
        captchaGeneration++;
      }
    }
  }
</script>

<header class="page-header"><h1>{t('Translate')}</h1></header>
<form class="translation-panel" onsubmit={submit}>
  <div class="translation-files">
    <div class="section-title">
      <h2>{t('Files')}</h2>
      <span class="format-label">SRT / VTT / ASS / SSA</span>
    </div>
    <section
      class="file-area"
      class:dragging
      aria-label={t('Subtitle files')}
      ondragenter={(event) => {
        event.preventDefault();
        if (!busy) {
          dragDepth++;
          dragging = true;
        }
      }}
      ondragover={(event) => event.preventDefault()}
      ondragleave={(event) => {
        event.preventDefault();
        if (--dragDepth <= 0) {
          dragging = false;
          dragDepth = 0;
        }
      }}
      ondrop={drop}
    >
      <input
        class="visually-hidden"
        type="file"
        multiple
        accept=".srt,.vtt,.ass,.ssa"
        bind:this={fileInput!}
        onchange={(event) => addFiles(event.currentTarget.files)}
        aria-label={t('Choose subtitle files')}
        disabled={busy}
      />
      {#if files.length}
        <ul class="file-list">
          {#each files as file, index (`${file.name}-${file.size}-${file.lastModified}`)}
            <li>
              <span class="file-type"><Icon name="file" size={19} /></span>
              <div class="file-info">
                <span class="file-name" title={file.name}>{file.name}</span><span class="file-size"
                  >{formatSize(file.size)}</span
                >
              </div>
              <button
                class="icon-button"
                type="button"
                disabled={busy}
                aria-label={t('Remove {filename}', { filename: file.name })}
                onclick={() => {
                  files = files.filter((_, position) => position !== index);
                }}><Icon name="close" size={16} /></button
              >
            </li>
          {/each}
        </ul>
        <div class="file-list-footer">
          <button
            class="text-button"
            type="button"
            onclick={() => fileInput.click()}
            disabled={busy}><Icon name="plus" size={15} />{t('Add files')}</button
          ><span class="muted">{formatSize(totalSize)}</span>
        </div>
      {:else}
        <button class="dropzone" type="button" disabled={busy} onclick={() => fileInput.click()}
          ><span class="upload-symbol"><Icon name="upload" size={24} /></span><strong
            >{t('Drop subtitle files here')}</strong
          ><span class="browse-link">{t('Choose files')}<Icon name="arrow" size={14} /></span
          ></button
        >
      {/if}
    </section>
  </div>
  <div class="translation-options">
    <div class="field-pair">
      <label
        >{t('Source language')}<input
          bind:value={source}
          maxlength="100"
          placeholder={t('Auto or English')}
        /></label
      ><label
        >{t('Provider')}<select bind:value={provider}
          >{#each Object.entries(appState.settings?.providers || {}) as [code, label]}<option
              value={code}>{label}</option
            >{/each}</select
        ></label
      >
    </div>
    <fieldset class="target-fieldset">
      <legend>{t('Target languages')}</legend>
      <div class="selected-languages">
        {#each targets as code}<button
            class="language-tag"
            type="button"
            aria-label={t('Remove {language}', { language: languageName(code) })}
            onclick={() => toggleTarget(code)}
            >{languageName(code)}<Icon name="close" size={13} /></button
          >{/each}
      </div>
      <details class="language-menu" bind:this={languageMenu!}>
        <summary>{t('Choose languages')}<Icon name="down" size={15} /></summary>
        <div class="language-options">
          {#each allLanguages as [code, label]}<label
              ><input
                type="checkbox"
                checked={targets.includes(code)}
                onchange={() => toggleTarget(code)}
              /><span>{label}</span></label
            >{/each}
        </div>
      </details>
    </fieldset>
    <details class="advanced-options">
      <summary>{t('Options')}<Icon name="down" size={15} /></summary>
      <div class="stack">
        {#if provider === 'anthropic' || provider === 'openai'}<label
            >{t('Model override')}<input
              bind:value={model}
              maxlength="200"
              placeholder={setting(`${provider}_model`)}
            /></label
          >{/if}
        <label
          >{t('File encoding')}<select bind:value={encoding}
            ><option value="utf-8">UTF-8</option><option value="utf-16">UTF-16</option><option
              value="gbk">GBK</option
            ><option value="big5">Big5</option><option value="shift_jis">Shift JIS</option><option
              value="cp1252">Windows-1252</option
            ><option value="latin-1">ISO-8859-1</option></select
          ></label
        >
      </div>
    </details>
    {#if !ready}<p class="provider-warning">
        <Icon name="alert" size={15} /><span
          >{t('This provider is not configured')}{#if appState.user?.role === 'admin'}
            <button class="text-button" type="button" onclick={() => navigate('settings')}
              >{t('Settings')}</button
            >{/if}</span
        >
      </p>{/if}
    {#if provider === 'echo'}<p class="muted small-text">
        {t('Offline test mode — no API calls')}
      </p>{/if}
    <Captcha
      action="upload"
      onToken={(value) => {
        captchaToken = value;
      }}
      generation={captchaGeneration}
    />
    {#if error}<p class="form-error" role="alert">{error}</p>{/if}
    <div class="translation-submit">
      <button
        class="primary"
        type="submit"
        disabled={busy ||
          !ready ||
          !files.length ||
          !targets.length ||
          (captchaRequired && !captchaToken)}
        >{busy ? t('Uploading…') : t('Translate')}<Icon name="arrow" size={17} /></button
      >
    </div>
  </div>
</form>
<section class="recent-section" id="recent-jobs">
  <div class="section-heading">
    <h2>{t('Recent translations')}</h2>
    <a class="text-button" href="#history">{t('View all')}<Icon name="arrow" size={14} /></a>
  </div>
  <JobList {onExpired} />
</section>
