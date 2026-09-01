const state = {
  user: null, settings: null, jobs: [], overviewJobs: [], users: [], timer: null, setup: false,
  authMode: 'login', authConfig: {
    registration_enabled: false,
    captcha: { provider: 'none', site_key: '', protected_actions: [] },
  },
  captchaWidgets: { auth: null, upload: null }, captchaLoaders: {},
  currentView: 'dashboard',
  i18n: { locale: document.body.dataset.locale || 'en', messages: {}, languages: {} },
};
const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];
const unsafeMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);
const supportedThemes = new Set(['system', 'light', 'dark']);
const systemTheme = window.matchMedia('(prefers-color-scheme: dark)');

function resolvedTheme(theme = document.documentElement.dataset.theme || 'system') {
  return theme === 'system' ? (systemTheme.matches ? 'dark' : 'light') : theme;
}

function applyTheme(theme) {
  const selected = supportedThemes.has(theme) ? theme : 'system';
  document.documentElement.dataset.theme = selected;
  const selector = $('#themeSelect');
  if (selector) selector.value = selected;
}

function t(message, values = {}) {
  const translated = state.i18n.messages[message] || message;
  return translated.replace(/\{([a-z_]+)\}/gi, (match, key) =>
    Object.hasOwn(values, key) ? String(values[key]) : match
  );
}

function languageName(code) {
  return state.i18n.languages[code] || code;
}

async function loadI18n() {
  const response = await fetch('/api/i18n', { credentials: 'same-origin' });
  if (!response.ok) throw new Error(`Request failed (${response.status})`);
  state.i18n = await response.json();
}

function cookie(name) {
  const prefix = `${encodeURIComponent(name)}=`;
  const item = document.cookie.split('; ').find(value => value.startsWith(prefix));
  return item ? decodeURIComponent(item.slice(prefix.length)) : '';
}

function toast(message) {
  const element = $('#toast');
  element.textContent = message;
  element.classList.add('show');
  setTimeout(() => element.classList.remove('show'), 3000);
}

async function api(url, options = {}) {
  const method = (options.method || 'GET').toUpperCase();
  const headers = new Headers(options.headers || {});
  if (unsafeMethods.has(method)) {
    const csrf = cookie('csrf_access_token');
    if (csrf) headers.set('X-CSRF-TOKEN', csrf);
  }
  const response = await fetch(url, { ...options, method, headers, credentials: 'same-origin' });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(data.error || t('Request failed ({status})', { status: response.status }));
    error.status = response.status;
    throw error;
  }
  return data;
}

function escapeHtml(value) {
  const node = document.createElement('div');
  node.textContent = String(value ?? '');
  return node.innerHTML;
}

function captchaRequired(action) {
  const captcha = state.authConfig.captcha || {};
  return captcha.provider !== 'none' && (captcha.protected_actions || []).includes(action);
}

const captchaSdkUrls = {
  turnstile: 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit',
  recaptcha: 'https://www.google.com/recaptcha/api.js?render=explicit',
  hcaptcha: 'https://js.hcaptcha.com/1/api.js?render=explicit&recaptchacompat=off',
};
const captchaGlobalNames = {
  turnstile: 'turnstile', recaptcha: 'grecaptcha', hcaptcha: 'hcaptcha',
};

function loadCaptchaSdk(provider) {
  if (window[captchaGlobalNames[provider]]) return Promise.resolve();
  if (state.captchaLoaders[provider]) return state.captchaLoaders[provider];
  state.captchaLoaders[provider] = new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = captchaSdkUrls[provider];
    script.async = true;
    script.defer = true;
    script.addEventListener('error', () => reject(new Error(t('Could not load CAPTCHA'))));
    script.addEventListener('load', () => {
      let checks = 0;
      const ready = () => {
        if (window[captchaGlobalNames[provider]]) return resolve();
        checks += 1;
        if (checks >= 50) return reject(new Error(t('Could not load CAPTCHA')));
        setTimeout(ready, 100);
      };
      ready();
    });
    document.head.append(script);
  });
  state.captchaLoaders[provider].catch(() => { delete state.captchaLoaders[provider]; });
  return state.captchaLoaders[provider];
}

function captchaApi(provider) {
  return window[captchaGlobalNames[provider]];
}

function resetCaptcha(slot) {
  const widget = state.captchaWidgets[slot];
  if (!widget) return;
  const apiObject = captchaApi(widget.provider);
  try { apiObject?.reset(widget.id); } catch (_error) { /* SDK owns reset errors. */ }
}

function removeCaptcha(slot) {
  const widget = state.captchaWidgets[slot];
  if (widget) {
    const apiObject = captchaApi(widget.provider);
    try {
      if (typeof apiObject?.remove === 'function') apiObject.remove(widget.id);
      else apiObject?.reset(widget.id);
    } catch (_error) { /* Replacing the container removes stale widget markup. */ }
  }
  state.captchaWidgets[slot] = null;
  $(`#${slot}Captcha`).replaceChildren();
}

async function renderCaptcha(slot, action) {
  const container = $(`#${slot}Captcha`);
  if (!captchaRequired(action)) {
    container.hidden = true;
    removeCaptcha(slot);
    return;
  }
  const { provider, site_key: siteKey } = state.authConfig.captcha;
  const theme = resolvedTheme();
  container.hidden = false;
  if (!siteKey) {
    container.textContent = t('CAPTCHA is temporarily unavailable');
    return;
  }
  const existing = state.captchaWidgets[slot];
  if (existing && existing.provider === provider && existing.theme === theme
      && (provider !== 'turnstile' || existing.action === action)) {
    resetCaptcha(slot);
    return;
  }
  removeCaptcha(slot);
  await loadCaptchaSdk(provider);
  const options = { sitekey: siteKey, theme };
  if (provider === 'turnstile') options.action = action;
  const id = captchaApi(provider).render(container, options);
  state.captchaWidgets[slot] = { id, provider, action, theme };
}

function captchaToken(slot, action) {
  if (!captchaRequired(action)) return '';
  const widget = state.captchaWidgets[slot];
  if (!widget) throw new Error(t('Complete the CAPTCHA challenge'));
  const token = captchaApi(widget.provider)?.getResponse(widget.id) || '';
  if (!token) throw new Error(t('Complete the CAPTCHA challenge'));
  return token;
}

async function refreshAuthConfiguration(status = null) {
  const configuration = status || await api('/api/auth/setup-status');
  state.authConfig = {
    registration_enabled: Boolean(configuration.registration_enabled),
    captcha: configuration.captcha || { provider: 'none', site_key: '', protected_actions: [] },
  };
  return configuration;
}

function showAuth(setup = false, mode = 'login') {
  state.setup = setup;
  state.authMode = setup ? 'setup' : mode;
  state.user = null;
  applyTheme('system');
  clearTimeout(state.timer);
  $('#appShell').hidden = true;
  $('#authView').hidden = false;
  const registering = state.authMode === 'register';
  $('#authTitle').textContent = setup ? t('Create the first administrator') :
    t(registering ? 'Create your account' : 'Sign in');
  $('#authEyebrow').textContent = setup ? t('Initial setup') : t('Authentication');
  $('#authDescription').textContent = setup
    ? t('This one-time account will manage users, provider keys, and all jobs.')
    : t(registering ? 'Choose a username and a strong password to join this workspace.' :
      'Use your Subtitle Translator account.');
  $('#authSubmit').textContent = setup ? t('Create administrator') :
    t(registering ? 'Create account' : 'Sign in');
  $('#authForm [name="password"]').autocomplete = setup || registering ?
    'new-password' : 'current-password';
  const confirmation = $('#confirmPasswordField');
  confirmation.hidden = !registering;
  confirmation.querySelector('input').required = registering;
  confirmation.querySelector('input').disabled = !registering;
  $('#authSwitch').hidden = setup || !state.authConfig.registration_enabled;
  $('#authSwitchPrompt').textContent = t(registering ? 'Already have an account?' : 'Need an account?');
  $('#authSwitchButton').textContent = t(registering ? 'Sign in' : 'Create one');
  $('#authError').textContent = '';
  renderCaptcha('auth', state.authMode).catch(error => { $('#authError').textContent = error.message; });
}

async function enterApp(user) {
  state.user = user;
  applyTheme(user.theme);
  $('#authView').hidden = true;
  $('#appShell').hidden = false;
  const admin = user.role === 'admin';
  $('#userName').textContent = user.username;
  $('#userRole').textContent = t(admin ? 'Administrator' : 'User');
  $('#userInitial').textContent = user.username.slice(0, 1);
  $('#adminNavButton').hidden = !admin;
  $('#allJobsLabel').hidden = !admin;
  $('#allJobs').checked = admin;
  showView(window.location.hash.slice(1) || 'dashboard', false);
  await Promise.all([loadSettings(), loadJobs(), admin ? loadUsers() : Promise.resolve()]);
  await renderCaptcha('upload', 'upload').catch(error => toast(error.message));
}

const viewLabels = {
  dashboard: ['Workspace overview', 'Dashboard'],
  translate: ['Translation workspace', 'New translation'],
  jobs: ['Activity', 'Translation history'],
  admin: ['Panel management', 'Administration'],
};

function showView(requestedView, updateHash = true) {
  const allowed = new Set(['dashboard', 'translate', 'jobs']);
  if (state.user?.role === 'admin') allowed.add('admin');
  const view = allowed.has(requestedView) ? requestedView : 'dashboard';
  state.currentView = view;
  $$('[data-view]').forEach(element => { element.hidden = element.dataset.view !== view; });
  $$('[data-view-button]').forEach(button => {
    const active = button.dataset.viewButton === view;
    button.classList.toggle('active', active);
    button.setAttribute('aria-current', active ? 'page' : 'false');
  });
  $('#viewEyebrow').textContent = t(viewLabels[view][0]);
  $('#viewTitle').textContent = t(viewLabels[view][1]);
  $('.topbar-action').hidden = view === 'translate';
  if (updateHash) window.history.replaceState(null, '', `#${view}`);
  window.scrollTo({ top: 0, behavior: 'smooth' });
}

function metricCard(label, value, note, accent = false) {
  return `<article class="metric-card${accent ? ' accent' : ''}">
    <span class="metric-label">${escapeHtml(label)}</span>
    <strong class="metric-value">${escapeHtml(value)}</strong>
    <span class="metric-note">${escapeHtml(note)}</span>
  </article>`;
}

function formatJobDate(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Intl.DateTimeFormat(state.i18n.locale, {
    dateStyle: 'medium', timeStyle: 'short',
  }).format(date);
}

function renderProviderSummaries() {
  if (!state.settings) return;
  const providers = Object.entries(state.settings.providers);
  const readyCount = providers.filter(([key]) => state.settings.configured[key]).length;
  $('#dashboardProviders').innerHTML = `
    <div class="provider-summary-row"><span>${escapeHtml(t('Providers ready'))}</span><strong>${readyCount}/${providers.length}</strong></div>
    <div class="provider-summary-row"><span>${escapeHtml(t('Default provider'))}</span><strong>${escapeHtml(state.settings.providers[state.settings.default_provider])}</strong></div>`;
  $('#adminProviders').innerHTML = providers.map(([key, label]) => {
    const ready = state.settings.configured[key];
    return `<div class="provider-admin-row">
      <div><strong>${escapeHtml(label)}</strong><small>${escapeHtml(key === state.settings.default_provider ? t('Panel default') : t('Available provider'))}</small></div>
      <span class="readiness${ready ? ' ready' : ''}">${escapeHtml(t(ready ? 'Configured' : 'Not set'))}</span>
    </div>`;
  }).join('');
}

function renderDashboard() {
  if (!state.user) return;
  const overviewJobs = state.overviewJobs;
  const active = overviewJobs.filter(job => ['queued', 'processing', 'canceling'].includes(job.status)).length;
  const completed = overviewJobs.filter(job => job.status === 'completed').length;
  const failed = overviewJobs.filter(job => job.status === 'failed').length;
  const scope = state.user.role === 'admin' ? t('Across all users') : t('In your workspace');
  $('#welcomeTitle').textContent = t('Welcome back, {username}', { username: state.user.username });
  $('#welcomeDescription').textContent = state.user.role === 'admin'
    ? t('Your panel-wide activity and access overview is ready.')
    : t('Here is what is happening in your translation workspace.');
  $('#dashboardMetrics').innerHTML = [
    metricCard(t('Total jobs'), overviewJobs.length, scope),
    metricCard(t('Active now'), active,
      active === 1 ? t('Translation in progress') : t('Translations in progress'), active > 0),
    metricCard(t('Completed'), completed, t('Ready or downloaded')),
    metricCard(t('Needs attention'), failed, t('Failed translations')),
  ].join('');
  $('#dashboardRecent').innerHTML = overviewJobs.length ? overviewJobs.slice(0, 5).map(job => `
    <div class="recent-row">
      <div><div class="recent-name" title="${escapeHtml(job.filename)}">${escapeHtml(job.filename)}</div>
      <div class="recent-meta">${escapeHtml(job.options.provider)} · ${escapeHtml(job.options.target_languages.map(languageName).join(', '))} · ${escapeHtml(formatJobDate(job.created_at))}</div></div>
      <span class="status ${escapeHtml(job.status)}">${escapeHtml(t(job.status))}</span>
    </div>`).join('') : `<div class="empty">${escapeHtml(t('No translations yet.'))}</div>`;
  renderProviderSummaries();
  renderAdminDashboard();
}

function renderAdminDashboard() {
  if (state.user?.role !== 'admin') return;
  const activeUsers = state.users.filter(user => user.active).length;
  const admins = state.users.filter(user => user.role === 'admin' && user.active).length;
  const activeJobs = state.overviewJobs.filter(job => ['queued', 'processing', 'canceling'].includes(job.status)).length;
  const providers = state.settings ? Object.keys(state.settings.providers) : [];
  const readyProviders = state.settings ? providers.filter(key => state.settings.configured[key]).length : 0;
  $('#adminMetrics').innerHTML = [
    metricCard(t('Total users'), state.users.length, t('{count} active accounts', { count: activeUsers })),
    metricCard(t('Active administrators'), admins, t('Protected panel access')),
    metricCard(t('Panel jobs'), state.overviewJobs.length, t('{count} currently active', { count: activeJobs }), activeJobs > 0),
    metricCard(t('Ready providers'), `${readyProviders}/${providers.length}`, t('Configured for translation')),
  ].join('');
}

async function submitAuth(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const button = $('#authSubmit');
  const payload = Object.fromEntries(new FormData(form).entries());
  $('#authError').textContent = '';
  try {
    if (state.authMode === 'register' && payload.password !== payload.confirm_password) {
      throw new Error(t('Passwords do not match'));
    }
    payload.captcha_token = captchaToken('auth', state.authMode);
    button.disabled = true;
    const endpoint = state.setup ? '/api/auth/setup' :
      state.authMode === 'register' ? '/api/auth/register' : '/api/auth/login';
    const data = await api(endpoint, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    form.reset();
    await enterApp(data.user);
  } catch (error) {
    $('#authError').textContent = error.message;
    resetCaptcha('auth');
  } finally {
    button.disabled = false;
  }
}

async function logout() {
  try { await api('/api/auth/logout', { method: 'POST' }); }
  catch (error) { if (error.status !== 401) toast(error.message); }
  const status = await refreshAuthConfiguration();
  showAuth(!status.configured);
}

async function saveTheme(event) {
  const selector = event.currentTarget;
  const previous = state.user?.theme || 'system';
  const theme = selector.value;
  applyTheme(theme);
  selector.disabled = true;
  try {
    const data = await api('/api/auth/me', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ theme }),
    });
    state.user = data.user;
    applyTheme(data.user.theme);
    toast(t('Theme updated'));
    renderCaptcha('upload', 'upload').catch(error => toast(error.message));
  } catch (error) {
    applyTheme(previous);
    toast(error.message);
  } finally {
    selector.disabled = false;
  }
}

function configureProviderSelect(selectElement, selected) {
  selectElement.innerHTML = Object.entries(state.settings.providers).map(([value, label]) =>
    `<option value="${escapeHtml(value)}" ${value === selected ? 'selected' : ''}>${escapeHtml(label)}</option>`
  ).join('');
}

async function loadSettings() {
  state.settings = await api('/api/settings');
  configureProviderSelect($('#provider'), state.settings.default_provider);
  configureProviderSelect($('#defaultProvider'), state.settings.default_provider);
  $('#sourceLanguage').value = state.settings.source_language;
  const defaults = state.settings.target_languages.split(',');
  $$('#languagePicker input').forEach(box => { box.checked = defaults.includes(box.value); });
  Object.entries(state.settings).forEach(([key, value]) => {
    const field = $(`#settingsForm [name="${key}"]`);
    if (field && !key.endsWith('_api_key')) field.value = value;
  });
  $$('.key-state').forEach(element => {
    const provider = element.dataset.provider;
    const ready = provider.startsWith('captcha-') ?
      state.settings.captcha_configured[provider.slice('captcha-'.length)] :
      state.settings.configured[provider];
    element.textContent = ready ? t('Configured') : t('Not set');
    element.classList.toggle('ready', ready);
    const clearButton = $(`.clear-key[data-provider="${element.dataset.provider}"]`);
    if (clearButton) clearButton.hidden = !ready;
  });
  updateProviderState();
  renderDashboard();
}

function updateProviderState() {
  const provider = $('#provider').value;
  const ready = state.settings?.configured?.[provider];
  $('#providerState').textContent = provider === 'echo' ? t('Offline test mode — no API calls') :
    ready ? t('{provider} is configured', { provider: $('#provider').selectedOptions[0].text }) :
      t('Ask an administrator to configure this provider');
  $('#modelField').style.display = ['anthropic', 'openai'].includes(provider) ? '' : 'none';
}

function selectedFiles() { return [...$('#fileInput').files]; }

function renderFiles() {
  $('#fileList').innerHTML = selectedFiles().map(file =>
    `<span class="file-pill">${escapeHtml(file.name)}</span>`
  ).join('');
}

async function submitTranslation(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const files = selectedFiles();
  const targets = $$('#languagePicker input:checked').map(element => element.value);
  if (!files.length) return toast(t('Choose at least one subtitle file'));
  if (!targets.length) return toast(t('Choose at least one target language'));
  const provider = $('#provider').value;
  if (!state.settings.configured[provider]) return toast(t('This provider is not configured'));
  const button = $('#submitButton');
  try {
    const token = captchaToken('upload', 'upload');
    button.disabled = true;
    const data = new FormData(form);
    data.delete('files');
    files.forEach(file => data.append('files', file));
    data.set('target_languages', targets.join(','));
    if (token) data.set('captcha_token', token);
    await api('/api/jobs', { method: 'POST', body: data });
    form.querySelector('[name="model"]').value = '';
    $('#fileInput').value = '';
    renderFiles();
    toast(files.length === 1 ? t('Translation queued') :
      t('{count} translations queued', { count: files.length }));
    await loadJobs();
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; resetCaptcha('upload'); }
}

function renderJobs() {
  const container = $('#jobs');
  if (!state.jobs.length) {
    container.innerHTML = `<div class="empty">${escapeHtml(t('No translations yet.'))}</div>`;
    return;
  }
  container.innerHTML = state.jobs.map(job => {
    const jobId = encodeURIComponent(job.id);
    const targetNames = job.options.target_languages.map(languageName).join(', ');
    const owner = job.owner !== undefined ? ` · ${escapeHtml(job.owner || t('deleted user'))}` : '';
    const outputLinks = job.outputs.map(output =>
      `<a href="/api/jobs/${jobId}/download/${encodeURIComponent(output.name)}">${escapeHtml(languageName(output.language))}</a>`
    ).join('');
    const primaryAction = job.status === 'completed' ?
      `<div class="download-actions"><a class="download" href="/api/jobs/${jobId}/download">${escapeHtml(t(job.outputs.length > 1 ? 'Download ZIP' : 'Download'))}</a>${job.outputs.length > 1 ? `<div class="language-downloads">${outputLinks}</div>` : ''}</div>` :
      `<span class="status ${escapeHtml(job.status)}">${escapeHtml(t(job.status))}</span>`;
    const cancelAction = ['queued', 'processing'].includes(job.status) ?
      `<button class="cancel-job" type="button" data-job-id="${jobId}">${escapeHtml(t('Cancel'))}</button>` :
      job.status === 'canceling' ? `<button class="cancel-job" type="button" disabled>${escapeHtml(t('Canceling…'))}</button>` : '';
    const deleteAction = ['completed', 'failed', 'canceled'].includes(job.status) ?
      `<button class="delete-job" type="button" data-job-id="${jobId}">${escapeHtml(t('Delete'))}</button>` : '';
    return `<article class="job">
      <div><div class="job-name" title="${escapeHtml(job.filename)}">${escapeHtml(job.filename)}</div>
      <div class="job-meta">${escapeHtml(job.options.provider)} · ${escapeHtml(targetNames)}${owner}</div></div>
      <div><div class="progress-track"><div class="progress-bar" style="width:${Number(job.progress)}%"></div></div>
      <div class="job-meta">${escapeHtml(job.stage || job.status)} · ${Number(job.progress)}%</div>
      ${job.error ? `<div class="job-error">${escapeHtml(job.error)}</div>` : ''}</div>
      <div class="job-actions">${primaryAction}${cancelAction}${deleteAction}</div></article>`;
  }).join('');
}

async function loadJobs() {
  try {
    const admin = state.user?.role === 'admin';
    const all = admin && $('#allJobs').checked;
    state.jobs = (await api(`/api/jobs${all ? '?all=1' : ''}`)).jobs;
    state.overviewJobs = admin && !all ? (await api('/api/jobs?all=1')).jobs : state.jobs;
    renderJobs();
    renderDashboard();
    const active = state.jobs.some(job => ['queued', 'processing', 'canceling'].includes(job.status));
    clearTimeout(state.timer);
    state.timer = setTimeout(loadJobs, active ? 1500 : 8000);
  } catch (error) {
    if (error.status === 401) showAuth(false); else toast(error.message);
  }
}

async function jobAction(event) {
  const cancelButton = event.target.closest('.cancel-job');
  const deleteButton = event.target.closest('.delete-job');
  const button = cancelButton || deleteButton;
  if (!button || button.disabled) return;
  const jobId = decodeURIComponent(button.dataset.jobId);
  const job = state.jobs.find(item => item.id === jobId);
  const action = cancelButton ? 'cancel' : 'delete';
  if (!job || !window.confirm(t(action === 'cancel' ? 'Cancel {filename}?' : 'Delete {filename}?', {
    filename: job.filename,
  }))) return;
  button.disabled = true;
  try {
    if (cancelButton) await api(`/api/jobs/${encodeURIComponent(jobId)}/cancel`, { method: 'POST' });
    else await api(`/api/jobs/${encodeURIComponent(jobId)}`, { method: 'DELETE' });
    await loadJobs();
    toast(t(cancelButton ? 'Cancellation requested' : 'Translation deleted'));
  } catch (error) { button.disabled = false; toast(error.message); }
}

async function saveSettings(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const payload = Object.fromEntries(new FormData(form).entries());
  try {
    state.settings = await api('/api/settings', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload),
    });
    form.querySelectorAll('input[type="password"]').forEach(input => { input.value = ''; });
    $('#settingsMessage').textContent = t('Saved');
    await Promise.all([loadSettings(), refreshAuthConfiguration()]);
    await renderCaptcha('upload', 'upload');
    setTimeout(() => { $('#settingsMessage').textContent = ''; $('#settingsDialog').close(); }, 600);
  } catch (error) { $('#settingsMessage').textContent = error.message; }
}

async function removeKey(event) {
  const button = event.target.closest('.clear-key');
  if (!button) return;
  const provider = button.dataset.provider;
  if (!window.confirm(t('Remove the saved secret for {provider}?', { provider }))) return;
  button.disabled = true;
  try {
    state.settings = await api(`/api/settings/keys/${encodeURIComponent(provider)}`, {
      method: 'DELETE',
    });
    await Promise.all([loadSettings(), refreshAuthConfiguration()]);
    await renderCaptcha('upload', 'upload');
    toast(t('Secret removed'));
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
}

async function loadUsers() {
  state.users = (await api('/api/users')).users;
  $('#userList').innerHTML = state.users.map(user => `
    <article class="user-row" data-user-id="${escapeHtml(user.id)}">
      <div><strong>${escapeHtml(user.username)}</strong><div class="job-meta">${escapeHtml(t('{count} jobs', { count: user.job_count }))} · ${escapeHtml(t(user.active ? 'active' : 'disabled'))}${user.locked ? ` · ${escapeHtml(t('locked'))}` : ''}</div></div>
      <select class="user-role" ${user.id === state.user.id ? 'disabled' : ''} aria-label="${escapeHtml(t('Role for {username}', { username: user.username }))}">
        <option value="user" ${user.role === 'user' ? 'selected' : ''}>${escapeHtml(t('User'))}</option>
        <option value="admin" ${user.role === 'admin' ? 'selected' : ''}>${escapeHtml(t('Administrator'))}</option>
      </select>
      <div class="user-actions">
        ${user.locked ? `<button class="unlock-user ghost small" type="button">${escapeHtml(t('Unlock'))}</button>` : ''}
        ${user.id !== state.user.id ? `<button class="reset-user ghost small" type="button">${escapeHtml(t('Reset password'))}</button><button class="toggle-user ghost small" type="button">${escapeHtml(t(user.active ? 'Disable' : 'Enable'))}</button><button class="remove-user ghost small" type="button">${escapeHtml(t('Delete'))}</button>` : ''}
      </div>
    </article>`).join('');
  renderAdminDashboard();
}

async function createUser(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const payload = Object.fromEntries(new FormData(form).entries());
  try {
    await api('/api/users', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload),
    });
    form.reset();
    await loadUsers();
    toast(t('User created'));
  } catch (error) { toast(error.message); }
}

async function userAction(event) {
  const row = event.target.closest('.user-row');
  if (!row) return;
  const userId = row.dataset.userId;
  const user = state.users.find(item => item.id === userId);
  let payload;
  let method = 'PATCH';
  if (event.type === 'change' && event.target.matches('.user-role')) {
    payload = { role: event.target.value };
  }
  else if (event.target.closest('.unlock-user')) payload = { unlock: true };
  else if (event.target.closest('.toggle-user')) payload = { active: !user.active };
  else if (event.target.closest('.reset-user')) {
    const password = window.prompt(t('New password for {username} (12+ characters):', {
      username: user.username,
    }));
    if (!password) return;
    payload = { password };
  } else if (event.target.closest('.remove-user')) {
    if (!window.confirm(t('Delete user {username}? Their finished jobs will remain for administrator cleanup.', {
      username: user.username,
    }))) return;
    method = 'DELETE';
  } else return;
  try {
    await api(`/api/users/${encodeURIComponent(userId)}`, {
      method, headers: payload ? { 'Content-Type': 'application/json' } : {},
      body: payload ? JSON.stringify(payload) : undefined,
    });
    await loadUsers();
    toast(t(method === 'DELETE' ? 'User deleted' : 'User updated'));
  } catch (error) { toast(error.message); await loadUsers(); }
}

async function initialize() {
  await loadI18n();
  const status = await refreshAuthConfiguration();
  if (!status.configured) return showAuth(true);
  try {
    const data = await api('/api/auth/me');
    await enterApp(data.user);
  } catch (error) { showAuth(false); }
}

$('#authForm').addEventListener('submit', submitAuth);
$('#authSwitchButton').addEventListener('click', () => {
  $('#authForm').reset();
  showAuth(false, state.authMode === 'register' ? 'login' : 'register');
});
$('#localeSelect').addEventListener('change', event => {
  const url = new URL(window.location.href);
  url.searchParams.set('lang', event.currentTarget.value);
  window.location.assign(url);
});
$('#logoutButton').addEventListener('click', logout);
$('#themeSelect').addEventListener('change', saveTheme);
$('#fileInput').addEventListener('change', renderFiles);
$('#provider').addEventListener('change', updateProviderState);
$('#translateForm').addEventListener('submit', submitTranslation);
$('#settingsForm').addEventListener('submit', saveSettings);
$('#settingsForm').addEventListener('click', removeKey);
function openSettings() {
  if (state.user?.role === 'admin') $('#settingsDialog').showModal();
}
$('#settingsButton').addEventListener('click', openSettings);
$('#providerSettingsButton').addEventListener('click', openSettings);
$('#closeSettings').addEventListener('click', () => $('#settingsDialog').close());
$('#createUserForm').addEventListener('submit', createUser);
$('#userList').addEventListener('click', userAction);
$('#userList').addEventListener('change', userAction);
$('#refreshButton').addEventListener('click', loadJobs);
$('#dashboardRefresh').addEventListener('click', async () => {
  await Promise.all([loadJobs(), state.user?.role === 'admin' ? loadUsers() : Promise.resolve()]);
});
$('#allJobs').addEventListener('change', loadJobs);
$('#jobs').addEventListener('click', jobAction);
document.addEventListener('click', event => {
  const control = event.target.closest('[data-view-button], [data-go-view]');
  if (control) showView(control.dataset.viewButton || control.dataset.goView);
});
window.addEventListener('hashchange', () => showView(window.location.hash.slice(1), false));
systemTheme.addEventListener('change', () => {
  if ((state.user?.theme || 'system') === 'system') {
    renderCaptcha(state.user ? 'upload' : 'auth', state.user ? 'upload' : state.authMode)
      .catch(error => toast(error.message));
  }
});
const dropzone = $('#dropzone');
['dragenter', 'dragover'].forEach(name => dropzone.addEventListener(name, event => {
  event.preventDefault(); dropzone.classList.add('dragging');
}));
['dragleave', 'drop'].forEach(name => dropzone.addEventListener(name, event => {
  event.preventDefault(); dropzone.classList.remove('dragging');
}));
dropzone.addEventListener('drop', event => {
  $('#fileInput').files = event.dataTransfer.files; renderFiles();
});

initialize().catch(error => showAuth(false));
