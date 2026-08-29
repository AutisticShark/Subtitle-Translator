const state = { user: null, settings: null, jobs: [], users: [], timer: null, setup: false };
const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];
const unsafeMethods = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

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
    const error = new Error(data.error || `Request failed (${response.status})`);
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
  $('#authTitle').textContent = setup ? 'Create the first administrator' : 'Sign in';
  $('#authEyebrow').textContent = setup ? 'Initial setup' : 'Authentication';
  $('#authDescription').textContent = setup
    ? 'This one-time account will manage users, provider keys, and all jobs.'
    : 'Use your Subtitle Translator account.';
  $('#authSubmit').textContent = setup ? 'Create administrator' : 'Sign in';
  $('#authForm [name="password"]').autocomplete = setup ? 'new-password' : 'current-password';
  $('#authError').textContent = '';
}

async function enterApp(user) {
  state.user = user;
  $('#authView').hidden = true;
  $('#appShell').hidden = false;
  const admin = user.role === 'admin';
  $('#userBadge').textContent = `${user.username} · ${user.role}`;
  $('#settingsButton').hidden = !admin;
  $('#usersButton').hidden = !admin;
  $('#allJobsLabel').hidden = !admin;
  await Promise.all([loadSettings(), loadJobs()]);
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
    element.textContent = ready ? 'Configured' : 'Not set';
    element.classList.toggle('ready', ready);
    const clearButton = $(`.clear-key[data-provider="${element.dataset.provider}"]`);
    if (clearButton) clearButton.hidden = !ready;
  });
  updateProviderState();
}

function updateProviderState() {
  const provider = $('#provider').value;
  const ready = state.settings?.configured?.[provider];
  $('#providerState').textContent = provider === 'echo' ? 'Offline test mode — no API calls' :
    ready ? `${$('#provider').selectedOptions[0].text} is configured` :
      'Ask an administrator to configure this provider';
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
  if (!files.length) return toast('Choose at least one subtitle file');
  if (!targets.length) return toast('Choose at least one target language');
  const provider = $('#provider').value;
  if (!state.settings.configured[provider]) return toast('This provider is not configured');
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
    toast(files.length === 1 ? 'Translation queued' : `${files.length} translations queued`);
    await loadJobs();
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
}

function renderJobs() {
  const container = $('#jobs');
  if (!state.jobs.length) {
    container.innerHTML = '<div class="empty">No translations yet.</div>';
    return;
  }
  container.innerHTML = state.jobs.map(job => {
    const jobId = encodeURIComponent(job.id);
    const targetNames = job.options.target_languages.join(', ');
    const owner = job.owner !== undefined ? ` · ${escapeHtml(job.owner || 'deleted user')}` : '';
    const outputLinks = job.outputs.map(output =>
      `<a href="/api/jobs/${jobId}/download/${encodeURIComponent(output.name)}">${escapeHtml(output.language)}</a>`
    ).join('');
    const primaryAction = job.status === 'completed' ?
      `<div class="download-actions"><a class="download" href="/api/jobs/${jobId}/download">Download${job.outputs.length > 1 ? ' ZIP' : ''}</a>${job.outputs.length > 1 ? `<div class="language-downloads">${outputLinks}</div>` : ''}</div>` :
      `<span class="status ${escapeHtml(job.status)}">${escapeHtml(job.status)}</span>`;
    const cancelAction = ['queued', 'processing'].includes(job.status) ?
      `<button class="cancel-job" type="button" data-job-id="${jobId}">Cancel</button>` :
      job.status === 'canceling' ? '<button class="cancel-job" type="button" disabled>Canceling…</button>' : '';
    const deleteAction = ['completed', 'failed', 'canceled'].includes(job.status) ?
      `<button class="delete-job" type="button" data-job-id="${jobId}">Delete</button>` : '';
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
    const all = state.user?.role === 'admin' && $('#allJobs').checked ? '?all=1' : '';
    state.jobs = (await api(`/api/jobs${all}`)).jobs;
    renderJobs();
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
  if (!job || !window.confirm(`${action === 'cancel' ? 'Cancel' : 'Delete'} ${job.filename}?`)) return;
  button.disabled = true;
  try {
    if (cancelButton) await api(`/api/jobs/${encodeURIComponent(jobId)}/cancel`, { method: 'POST' });
    else await api(`/api/jobs/${encodeURIComponent(jobId)}`, { method: 'DELETE' });
    await loadJobs();
    toast(cancelButton ? 'Cancellation requested' : 'Translation deleted');
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
    $('#settingsMessage').textContent = 'Saved';
    await loadSettings();
    setTimeout(() => { $('#settingsMessage').textContent = ''; $('#settingsDialog').close(); }, 600);
  } catch (error) { $('#settingsMessage').textContent = error.message; }
}

async function removeKey(event) {
  const button = event.target.closest('.clear-key');
  if (!button) return;
  const provider = button.dataset.provider;
  if (!window.confirm(`Remove the saved ${provider} API key?`)) return;
  button.disabled = true;
  try {
    state.settings = await api(`/api/settings/keys/${encodeURIComponent(provider)}`, {
      method: 'DELETE',
    });
    await loadSettings();
    toast('API key removed');
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
}

async function loadUsers() {
  state.users = (await api('/api/users')).users;
  $('#userList').innerHTML = state.users.map(user => `
    <article class="user-row" data-user-id="${escapeHtml(user.id)}">
      <div><strong>${escapeHtml(user.username)}</strong><div class="job-meta">${user.job_count} jobs · ${user.active ? 'active' : 'disabled'}${user.locked ? ' · locked' : ''}</div></div>
      <select class="user-role" ${user.id === state.user.id ? 'disabled' : ''} aria-label="Role for ${escapeHtml(user.username)}">
        <option value="user" ${user.role === 'user' ? 'selected' : ''}>User</option>
        <option value="admin" ${user.role === 'admin' ? 'selected' : ''}>Administrator</option>
      </select>
      <div class="user-actions">
        ${user.locked ? '<button class="unlock-user ghost small" type="button">Unlock</button>' : ''}
        ${user.id !== state.user.id ? `<button class="reset-user ghost small" type="button">Reset password</button><button class="toggle-user ghost small" type="button">${user.active ? 'Disable' : 'Enable'}</button><button class="remove-user ghost small" type="button">Delete</button>` : ''}
      </div>
    </article>`).join('');
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
    toast('User created');
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
    const password = window.prompt(`New password for ${user.username} (12+ characters):`);
    if (!password) return;
    payload = { password };
  } else if (event.target.closest('.remove-user')) {
    if (!window.confirm(`Delete user ${user.username}? Their finished jobs will remain for administrator cleanup.`)) return;
    method = 'DELETE';
  } else return;
  try {
    await api(`/api/users/${encodeURIComponent(userId)}`, {
      method, headers: payload ? { 'Content-Type': 'application/json' } : {},
      body: payload ? JSON.stringify(payload) : undefined,
    });
    await loadUsers();
    toast(method === 'DELETE' ? 'User deleted' : 'User updated');
  } catch (error) { toast(error.message); await loadUsers(); }
}

async function initialize() {
  const status = await api('/api/auth/setup-status');
  if (!status.configured) return showAuth(true);
  try {
    const data = await api('/api/auth/me');
    await enterApp(data.user);
  } catch (error) { showAuth(false); }
}

$('#authForm').addEventListener('submit', submitAuth);
$('#logoutButton').addEventListener('click', logout);
$('#fileInput').addEventListener('change', renderFiles);
$('#provider').addEventListener('change', updateProviderState);
$('#translateForm').addEventListener('submit', submitTranslation);
$('#settingsForm').addEventListener('submit', saveSettings);
$('#settingsForm').addEventListener('click', removeKey);
$('#settingsButton').addEventListener('click', () => $('#settingsDialog').showModal());
$('#closeSettings').addEventListener('click', () => $('#settingsDialog').close());
$('#usersButton').addEventListener('click', async () => { $('#usersDialog').showModal(); await loadUsers(); });
$('#closeUsers').addEventListener('click', () => $('#usersDialog').close());
$('#createUserForm').addEventListener('submit', createUser);
$('#userList').addEventListener('click', userAction);
$('#userList').addEventListener('change', userAction);
$('#refreshButton').addEventListener('click', loadJobs);
$('#allJobs').addEventListener('change', loadJobs);
$('#jobs').addEventListener('click', jobAction);
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
