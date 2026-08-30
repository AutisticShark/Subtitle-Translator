const state = {
  user: null, settings: null, jobs: [], overviewJobs: [], users: [], timer: null, setup: false,
  currentView: 'dashboard',
  i18n: { locale: document.body.dataset.locale || 'en', messages: {}, languages: {} },
};
const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];
const unsafeMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

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

function showAuth(setup = false) {
  state.setup = setup;
  state.user = null;
  clearTimeout(state.timer);
  $('#appShell').hidden = true;
  $('#authView').hidden = false;
  $('#authTitle').textContent = setup ? t('Create the first administrator') : t('Sign in');
  $('#authEyebrow').textContent = setup ? t('Initial setup') : t('Authentication');
  $('#authDescription').textContent = setup
    ? t('This one-time account will manage users, provider keys, and all jobs.')
    : t('Use your Subtitle Translator account.');
  $('#authSubmit').textContent = setup ? t('Create administrator') : t('Sign in');
  $('#authForm [name="password"]').autocomplete = setup ? 'new-password' : 'current-password';
  $('#authError').textContent = '';
}

async function enterApp(user) {
  state.user = user;
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
  button.disabled = true;
  $('#authError').textContent = '';
  try {
    const endpoint = state.setup ? '/api/auth/setup' : '/api/auth/login';
    const data = await api(endpoint, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    form.reset();
    await enterApp(data.user);
  } catch (error) {
    $('#authError').textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

async function logout() {
  try { await api('/api/auth/logout', { method: 'POST' }); }
  catch (error) { if (error.status !== 401) toast(error.message); }
  const status = await api('/api/auth/setup-status');
  showAuth(!status.configured);
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
    const ready = state.settings.configured[element.dataset.provider];
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
  button.disabled = true;
  try {
    const data = new FormData(form);
    data.delete('files');
    files.forEach(file => data.append('files', file));
    data.set('target_languages', targets.join(','));
    await api('/api/jobs', { method: 'POST', body: data });
    form.querySelector('[name="model"]').value = '';
    $('#fileInput').value = '';
    renderFiles();
    toast(files.length === 1 ? t('Translation queued') :
      t('{count} translations queued', { count: files.length }));
    await loadJobs();
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
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
    await loadSettings();
    setTimeout(() => { $('#settingsMessage').textContent = ''; $('#settingsDialog').close(); }, 600);
  } catch (error) { $('#settingsMessage').textContent = error.message; }
}

async function removeKey(event) {
  const button = event.target.closest('.clear-key');
  if (!button) return;
  const provider = button.dataset.provider;
  if (!window.confirm(t('Remove the saved {provider} API key?', { provider }))) return;
  button.disabled = true;
  try {
    state.settings = await api(`/api/settings/keys/${encodeURIComponent(provider)}`, {
      method: 'DELETE',
    });
    await loadSettings();
    toast(t('API key removed'));
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
  const status = await api('/api/auth/setup-status');
  if (!status.configured) return showAuth(true);
  try {
    const data = await api('/api/auth/me');
    await enterApp(data.user);
  } catch (error) { showAuth(false); }
}

$('#authForm').addEventListener('submit', submitAuth);
$('#localeSelect').addEventListener('change', event => {
  const url = new URL(window.location.href);
  url.searchParams.set('lang', event.currentTarget.value);
  window.location.assign(url);
});
$('#logoutButton').addEventListener('click', logout);
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
