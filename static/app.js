const state = { settings: null, jobs: [], timer: null };
const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];

function toast(message) {
  const el = $('#toast');
  el.textContent = message;
  el.classList.add('show');
  setTimeout(() => el.classList.remove('show'), 2600);
}

async function api(url, options = {}) {
  const response = await fetch(url, options);
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
  return data;
}

function configureProviderSelect(select, selected) {
  const labels = { anthropic: 'Anthropic', openai: 'OpenAI-compatible', deepl: 'DeepL', echo: 'Echo (offline test)' };
  select.innerHTML = Object.entries(labels).map(([value, label]) =>
    `<option value="${value}" ${value === selected ? 'selected' : ''}>${label}</option>`).join('');
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
  $$('.key-state').forEach(el => {
    const ready = state.settings.configured[el.dataset.provider];
    el.textContent = ready ? 'Configured' : 'Not set';
    el.classList.toggle('ready', ready);
  });
  updateProviderState();
}

function updateProviderState() {
  const provider = $('#provider').value;
  const ready = state.settings?.configured?.[provider];
  $('#providerState').textContent = provider === 'echo' ? 'Offline test mode — no API calls' :
    ready ? `${$('#provider').selectedOptions[0].text} is configured` : 'Add this provider’s API key in Settings';
  $('#modelField').style.display = ['anthropic', 'openai'].includes(provider) ? '' : 'none';
}

function selectedFiles() {
  return [...$('#fileInput').files];
}

function renderFiles() {
  const files = selectedFiles();
  $('#fileList').innerHTML = files.map(file => `<span class="file-pill">${escapeHtml(file.name)}</span>`).join('');
}

function escapeHtml(value) {
  const node = document.createElement('div');
  node.textContent = String(value);
  return node.innerHTML;
}

async function submitTranslation(event) {
  event.preventDefault();
  const files = selectedFiles();
  const targets = $$('#languagePicker input:checked').map(el => el.value);
  if (!files.length) return toast('Choose at least one subtitle file');
  if (!targets.length) return toast('Choose at least one target language');
  const provider = $('#provider').value;
  if (!state.settings.configured[provider]) return toast('Configure this provider first');
  const button = $('#submitButton');
  button.disabled = true;
  try {
    const data = new FormData(event.currentTarget);
    data.delete('files');
    files.forEach(file => data.append('files', file));
    data.set('target_languages', targets.join(','));
    await api('/api/jobs', { method: 'POST', body: data });
    event.currentTarget.querySelector('[name="model"]').value = '';
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
    const targetNames = job.options.target_languages.join(', ');
    const outputLinks = job.outputs.map(output =>
      `<a href="/api/jobs/${job.id}/download/${encodeURIComponent(output.name)}">${escapeHtml(output.language)}</a>`).join('');
    const action = job.status === 'completed' ?
      `<div class="download-actions"><a class="download" href="/api/jobs/${job.id}/download">Download${job.outputs.length > 1 ? ' ZIP' : ''}</a>${job.outputs.length > 1 ? `<div class="language-downloads">${outputLinks}</div>` : ''}</div>` :
      `<span class="status ${job.status}">${job.status}</span>`;
    return `<article class="job">
      <div><div class="job-name" title="${escapeHtml(job.filename)}">${escapeHtml(job.filename)}</div>
      <div class="job-meta">${escapeHtml(job.options.provider)} · ${escapeHtml(targetNames)}</div></div>
      <div><div class="progress-track"><div class="progress-bar" style="width:${job.progress}%"></div></div>
      <div class="job-meta">${escapeHtml(job.stage || job.status)} · ${job.progress}%</div>
      ${job.error ? `<div class="job-error">${escapeHtml(job.error)}</div>` : ''}</div>${action}</article>`;
  }).join('');
}

async function loadJobs() {
  try {
    state.jobs = (await api('/api/jobs')).jobs;
    renderJobs();
    const active = state.jobs.some(job => ['queued', 'processing'].includes(job.status));
    clearTimeout(state.timer);
    state.timer = setTimeout(loadJobs, active ? 1500 : 8000);
  } catch (error) { toast(error.message); }
}

async function saveSettings(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const payload = Object.fromEntries(form.entries());
  try {
    state.settings = await api('/api/settings', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
    });
    event.currentTarget.querySelectorAll('input[type="password"]').forEach(input => { input.value = ''; });
    $('#settingsMessage').textContent = 'Saved';
    await loadSettings();
    setTimeout(() => { $('#settingsMessage').textContent = ''; $('#settingsDialog').close(); }, 600);
  } catch (error) { $('#settingsMessage').textContent = error.message; }
}

$('#fileInput').addEventListener('change', renderFiles);
$('#provider').addEventListener('change', updateProviderState);
$('#translateForm').addEventListener('submit', submitTranslation);
$('#settingsForm').addEventListener('submit', saveSettings);
$('#settingsButton').addEventListener('click', () => $('#settingsDialog').showModal());
$('#closeSettings').addEventListener('click', () => $('#settingsDialog').close());
$('#refreshButton').addEventListener('click', loadJobs);
const dropzone = $('#dropzone');
['dragenter', 'dragover'].forEach(name => dropzone.addEventListener(name, event => { event.preventDefault(); dropzone.classList.add('dragging'); }));
['dragleave', 'drop'].forEach(name => dropzone.addEventListener(name, event => { event.preventDefault(); dropzone.classList.remove('dragging'); }));
dropzone.addEventListener('drop', event => { $('#fileInput').files = event.dataTransfer.files; renderFiles(); });

Promise.all([loadSettings(), loadJobs()]).catch(error => toast(error.message));
