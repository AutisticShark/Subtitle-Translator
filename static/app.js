const JOBS_PAGE_SIZE = 50;
const RECENT_JOBS = 5;
const POLL_ACTIVE_MS = 1500;
const POLL_IDLE_MS = 8000;
const POLL_MAX_BACKOFF_MS = 60000;
const activeStatuses = ['queued', 'processing', 'canceling'];

function emptyJobList() {
  return { jobs: [], total: 0, counts: {}, offset: 0, has_more: false };
}

const state = {
  user: null, settings: null, jobs: [], users: [], timer: null, setup: false,
  // history is the visible page; overview feeds the dashboard metrics and recent list.
  history: emptyJobList(), overview: emptyJobList(), jobsOffset: 0,
  // session changes on sign-in/out so stale responses from another account are dropped.
  session: 0, jobsRequest: 0, jobsController: null, pollFailures: 0,
  authMode: 'login', authConfig: {
    registration_enabled: false,
    captcha: { provider: 'none', site_key: '', protected_actions: [] },
  },
  captchaWidgets: { auth: null, upload: null }, captchaLoaders: {},
  captchaRenders: { auth: 0, upload: 0 },
  currentView: 'dashboard',
  loginChallenge: null, enrollmentChallenge: null, managementChallenge: null,
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

// Like api(), but resolves to undefined (never data or an error) when the user signed
// out or switched accounts while the request was in flight.
async function sessionApi(url, options = {}) {
  const session = state.session;
  try {
    const data = await api(url, options);
    return session === state.session ? data : undefined;
  } catch (error) {
    if (session !== state.session) return undefined;
    throw error;
  }
}

const htmlEscapes = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

// Safe for element text and for quoted attribute values.
function escapeHtml(value) {
  return String(value ?? '').replace(/[&<>"']/g, character => htmlEscapes[character]);
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

function captchaApi(provider) {
  return window[captchaGlobalNames[provider]];
}

// reCAPTCHA's explicit-render api.js defines a grecaptcha stub before its main
// script adds render(), so the global alone does not mean the SDK is usable.
function captchaSdkReady(provider) {
  return typeof captchaApi(provider)?.render === 'function';
}

function loadCaptchaSdk(provider) {
  if (captchaSdkReady(provider)) return Promise.resolve();
  if (state.captchaLoaders[provider]) return state.captchaLoaders[provider];
  state.captchaLoaders[provider] = new Promise((resolve, reject) => {
    const fail = () => reject(new Error(t('Could not load CAPTCHA')));
    let checks = 0;
    const ready = () => {
      if (captchaSdkReady(provider)) {
        const sdk = captchaApi(provider);
        if (provider === 'recaptcha' && typeof sdk.ready === 'function') sdk.ready(resolve);
        else resolve();
        return;
      }
      checks += 1;
      if (checks >= 150) return fail();
      setTimeout(ready, 100);
    };
    const src = captchaSdkUrls[provider];
    if ([...document.scripts].some(script => script.src === src)) {
      ready(); // A previous attempt already inserted the SDK; keep waiting for it.
      return;
    }
    const script = document.createElement('script');
    script.src = src;
    script.async = true;
    script.defer = true;
    script.addEventListener('error', () => { script.remove(); fail(); });
    script.addEventListener('load', ready);
    document.head.append(script);
  });
  state.captchaLoaders[provider].catch(() => { delete state.captchaLoaders[provider]; });
  return state.captchaLoaders[provider];
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
  // Each call supersedes earlier ones for this slot, so only the newest renders.
  const generation = state.captchaRenders[slot] + 1;
  state.captchaRenders[slot] = generation;
  const superseded = () => state.captchaRenders[slot] !== generation;
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
    removeCaptcha(slot);
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
  try {
    await loadCaptchaSdk(provider);
  } catch (error) {
    if (superseded()) return;
    throw error;
  }
  if (superseded()) return;
  removeCaptcha(slot);
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

// Start a new session generation: stop polling and make in-flight responses stale.
function endSession() {
  state.session += 1;
  state.jobsRequest += 1;
  state.jobsController?.abort();
  state.jobsController = null;
  clearTimeout(state.timer);
  state.timer = null;
  state.pollFailures = 0;
}

// Remove every trace of the previous account from memory and the hidden app shell.
function clearAccountData() {
  state.user = null;
  state.settings = null;
  state.jobs = [];
  state.users = [];
  state.history = emptyJobList();
  state.overview = emptyJobList();
  state.jobsOffset = 0;
  ['#jobs', '#jobsPageStatus', '#dashboardMetrics', '#dashboardRecent', '#dashboardProviders',
    '#adminMetrics', '#adminProviders', '#userList', '#fileList', '#provider', '#defaultProvider',
    '#providerState', '#mfaStatus', '#userName', '#userRole', '#userInitial', '#settingsMessage',
  ].forEach(selector => $(selector).replaceChildren());
  $('#jobsPager').hidden = true;
  $('#allJobs').checked = false;
  const dialog = $('#settingsDialog');
  if (dialog.open) dialog.close();
  ['#settingsForm', '#translateForm', '#createUserForm'].forEach(selector => $(selector).reset());
}

function showAuth(setup = false, mode = 'login') {
  endSession();
  clearAccountData();
  clearMfaSecrets();
  state.loginChallenge = null;
  $('#authForm').hidden = false;
  $('#mfaLoginForm').hidden = true;
  state.setup = setup;
  state.authMode = setup ? 'setup' : mode;
  applyTheme('system');
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
  endSession();
  const session = state.session;
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
  state.jobsOffset = 0;
  showView(window.location.hash.slice(1) || 'dashboard', false);
  await Promise.all([loadSettings(), loadJobs(), admin ? loadUsers() : Promise.resolve()]);
  if (session !== state.session) return;
  await renderCaptcha('upload', 'upload').catch(error => toast(error.message));
}

const viewLabels = {
  security: ['Account preferences', 'Account security'],
  dashboard: ['Workspace overview', 'Dashboard'],
  translate: ['Translation workspace', 'New translation'],
  jobs: ['Activity', 'Translation history'],
  admin: ['Panel management', 'Administration'],
};

function showView(requestedView, updateHash = true) {
  const allowed = new Set(['dashboard', 'translate', 'jobs', 'security']);
  if (state.user?.role === 'admin') allowed.add('admin');
  const view = allowed.has(requestedView) ? requestedView : 'dashboard';
  if (state.currentView === 'security' && view !== 'security') clearMfaSecrets();
  if (view === 'security') loadMfa().catch(error => { $('#mfaSecurityError').textContent = error.message; });
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

function activeJobCount(counts = {}) {
  return activeStatuses.reduce((sum, status) => sum + (counts[status] || 0), 0);
}

function providerLabel(key) {
  return state.settings?.providers?.[key] || key;
}

function jobStatusText(job) {
  // Stages arrive localized; a queued job has no stage yet, so localize its status.
  return job.stage || t(job.status);
}

function renderDashboard() {
  if (!state.user) return;
  const { jobs: recentJobs, total, counts } = state.overview;
  const active = activeJobCount(counts);
  const completed = counts.completed || 0;
  const failed = counts.failed || 0;
  const scope = state.user.role === 'admin' ? t('Across all users') : t('In your workspace');
  $('#welcomeTitle').textContent = t('Welcome back, {username}', { username: state.user.username });
  $('#welcomeDescription').textContent = state.user.role === 'admin'
    ? t('Your panel-wide activity and access overview is ready.')
    : t('Here is what is happening in your translation workspace.');
  $('#dashboardMetrics').innerHTML = [
    metricCard(t('Total jobs'), total, scope),
    metricCard(t('Active now'), active,
      active === 1 ? t('Translation in progress') : t('Translations in progress'), active > 0),
    metricCard(t('Completed'), completed, t('Ready or downloaded')),
    metricCard(t('Needs attention'), failed, t('Failed translations')),
  ].join('');
  $('#dashboardRecent').innerHTML = recentJobs.length ? recentJobs.slice(0, RECENT_JOBS).map(job => `
    <div class="recent-row">
      <div><div class="recent-name" title="${escapeHtml(job.filename)}">${escapeHtml(job.filename)}</div>
      <div class="recent-meta">${escapeHtml(providerLabel(job.options.provider))} · ${escapeHtml(job.options.target_languages.map(languageName).join(', '))} · ${escapeHtml(formatJobDate(job.created_at))}</div></div>
      <span class="status ${escapeHtml(job.status)}">${escapeHtml(t(job.status))}</span>
    </div>`).join('') : `<div class="empty">${escapeHtml(t('No translations yet.'))}</div>`;
  renderProviderSummaries();
  renderAdminDashboard();
}

function renderAdminDashboard() {
  if (state.user?.role !== 'admin') return;
  const activeUsers = state.users.filter(user => user.active).length;
  const admins = state.users.filter(user => user.role === 'admin' && user.active).length;
  const activeJobs = activeJobCount(state.overview.counts);
  const providers = state.settings ? Object.keys(state.settings.providers) : [];
  const readyProviders = state.settings ? providers.filter(key => state.settings.configured[key]).length : 0;
  $('#adminMetrics').innerHTML = [
    metricCard(t('Total users'), state.users.length, t('{count} active accounts', { count: activeUsers })),
    metricCard(t('Active administrators'), admins, t('Protected panel access')),
    metricCard(t('Panel jobs'), state.overview.total, t('{count} currently active', { count: activeJobs }), activeJobs > 0),
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
    if (data.mfa_required) {
      state.loginChallenge = data;
      $('#authForm').hidden = true;
      $('#authSwitch').hidden = true;
      $('#mfaLoginForm').hidden = false;
      $('#mfaLoginResend').hidden = data.method !== 'email';
      $('#authTitle').textContent = t('Multi-factor authentication');
      $('#mfaLoginDescription').textContent = data.method === 'email'
        ? t('Enter the code sent to your email, or a recovery code.')
        : t('Enter a new code from your authenticator app, or a recovery code.');
      $('#mfaLoginError').textContent = data.warning || '';
      $('#mfaLoginForm [name="code"]').focus();
      return;
    }
    await enterApp(data.user);
  } catch (error) {
    $('#authError').textContent = error.message;
    resetCaptcha('auth');
  } finally {
    button.disabled = false;
  }
}

function mfaPost(path, payload) {
  return api(`/api/auth/mfa${path}`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload),
  });
}

function clearMfaSecrets() {
  state.enrollmentChallenge = null;
  state.managementChallenge = null;
  $('#mfaQr').removeAttribute('src');
  $('#mfaSecret').value = '';
  $('#mfaRecoveryCodes').textContent = '';
  $('#mfaRecovery').hidden = true;
  $('#mfaConfirmForm').hidden = true;
  $('#mfaSetupForm').reset();
  $('#mfaManageForm').reset();
  $('#mfaConfirmForm').reset();
  $('#mfaLoginForm').reset();
  $('#mfaSecurityError').textContent = '';
  updateMfaMethod();
}

function updateMfaMethod() {
  const email = $('#mfaMethod').value === 'email';
  $('#mfaEmailField').hidden = !email;
  $('#mfaEmailField input').disabled = !email;
  $('#mfaEmailField input').required = email;
}

async function loadMfa() {
  const data = await sessionApi('/api/auth/mfa');
  if (!data) return;
  $('#mfaStatus').textContent = data.method
    ? t('MFA enabled: {method}. Recovery codes remaining: {count}.', {
      method: data.method === 'totp' ? t('Authenticator app') : data.email,
      count: data.recovery_remaining,
    }) : t('MFA is not enabled');
  if (!data.enrollment_available) $('#mfaStatus').textContent = t('Configure JWT_SECRET_KEY before saving secrets');
  $('#mfaSetupForm').hidden = Boolean(data.method) || !data.enrollment_available || Boolean(state.enrollmentChallenge);
  $('#mfaManageForm').hidden = !data.method;
  $('#mfaManageEmail').hidden = data.method !== 'email';
  $('#mfaMethod option[value="email"]').disabled = !data.email_available;
  if (!data.email_available && !data.method) {
    $('#mfaStatus').textContent += `. ${t('Email verification is unavailable; ask an administrator to configure email delivery')}`;
  }
}

async function submitMfaLogin(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector('[type="submit"]');
  button.disabled = true;
  $('#mfaLoginError').textContent = '';
  try {
    const data = await mfaPost('/verify', {
      challenge_token: state.loginChallenge?.challenge_token,
      code: new FormData(form).get('code'),
    });
    state.loginChallenge = null;
    form.reset();
    await enterApp(data.user);
  } catch (error) { $('#mfaLoginError').textContent = error.message; }
  finally { button.disabled = false; }
}

async function startMfaSetup(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector('[type="submit"]');
  button.disabled = true;
  $('#mfaSecurityError').textContent = '';
  try {
    const data = await mfaPost('/setup', Object.fromEntries(new FormData(form)));
    state.enrollmentChallenge = { challenge_token: data.challenge_token };
    form.reset();
    form.hidden = true;
    $('#mfaConfirmForm').hidden = false;
    $('#mfaAuthenticator').hidden = data.method !== 'totp';
    $('#mfaSetupResend').hidden = data.method !== 'email';
    $('#mfaEnrollmentNotice').textContent = data.warning || (data.method === 'email'
      ? t('Enter the six-digit email code within 5 minutes to enable MFA.')
      : t('Setup expires in 10 minutes. MFA starts only after you verify the code.'));
    if (data.method === 'totp') {
      $('#mfaQr').src = data.qr_code;
      $('#mfaSecret').value = data.secret;
    }
    $('#mfaConfirmForm [name="code"]').focus();
  } catch (error) { $('#mfaSecurityError').textContent = error.message; }
  finally { button.disabled = false; }
}

function showRecoveryCodes(data) {
  state.user = data.user;
  clearMfaSecrets();
  if (data.recovery_codes) {
    $('#mfaRecoveryCodes').textContent = data.recovery_codes.join('\n');
    $('#mfaRecovery').hidden = false;
  }
}

async function confirmMfaSetup(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const button = form.querySelector('[type="submit"]');
  button.disabled = true;
  $('#mfaSecurityError').textContent = '';
  try {
    const data = await mfaPost('/confirm', {
      challenge_token: state.enrollmentChallenge?.challenge_token,
      code: new FormData(form).get('code'),
    });
    showRecoveryCodes(data);
    await loadMfa();
  } catch (error) { $('#mfaSecurityError').textContent = error.message; }
  finally { button.disabled = false; }
}

async function manageMfa(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const action = event.submitter?.value;
  if (!['disable', 'recovery'].includes(action)) return;
  const buttons = [...form.querySelectorAll('button')];
  buttons.forEach(button => { button.disabled = true; });
  $('#mfaSecurityError').textContent = '';
  try {
    const data = await mfaPost(`/${action}`, {
      ...Object.fromEntries(new FormData(form)),
      challenge_token: state.managementChallenge?.challenge_token,
    });
    showRecoveryCodes(data);
    await loadMfa();
    toast(t('Account security updated'));
  } catch (error) { $('#mfaSecurityError').textContent = error.message; }
  finally { buttons.forEach(button => { button.disabled = false; }); }
}

async function resendMfa(slot, button) {
  const errorElement = slot === 'loginChallenge' ? $('#mfaLoginError') : $('#mfaSecurityError');
  button.disabled = true;
  try {
    const data = await mfaPost('/resend', { challenge_token: state[slot]?.challenge_token });
    state[slot] = data;
    errorElement.textContent = data.warning || t('Verification email sent');
  } catch (error) { errorElement.textContent = error.message; }
  finally { button.disabled = false; }
}

async function sendManagementEmail(event) {
  const button = event.currentTarget;
  const password = $('#mfaManageForm [name="password"]');
  if (!password.reportValidity()) return;
  button.disabled = true;
  try {
    const data = await mfaPost('/email', { password: password.value });
    state.managementChallenge = data;
    $('#mfaSecurityError').textContent = data.warning || t('Verification email sent');
  } catch (error) { $('#mfaSecurityError').textContent = error.message; }
  finally { button.disabled = false; }
}

async function logout() {
  endSession(); // Stop polling before the token is revoked.
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

function populateSettingsFields() {
  if (!state.settings) return;
  configureProviderSelect($('#defaultProvider'), state.settings.default_provider);
  Object.entries(state.settings).forEach(([key, value]) => {
    const field = $(`#settingsForm [name="${key}"]`);
    if (field && field.type !== 'password' && !key.endsWith('_api_key')) field.value = value;
  });
}

function clearSettingsSecrets() {
  $$('#settingsForm input[type="password"]').forEach(input => { input.value = ''; });
}

// Discard abandoned edits and typed secrets, then show the saved configuration.
function resetSettingsForm() {
  $('#settingsForm').reset();
  clearSettingsSecrets();
  populateSettingsFields();
  $('#settingsMessage').textContent = '';
}

async function loadSettings() {
  const settings = await sessionApi('/api/settings');
  if (!settings) return;
  state.settings = settings;
  configureProviderSelect($('#provider'), state.settings.default_provider);
  $('#sourceLanguage').value = state.settings.source_language;
  const defaults = state.settings.target_languages.split(',');
  $$('#languagePicker input').forEach(box => { box.checked = defaults.includes(box.value); });
  populateSettingsFields();
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
  renderJobs(); // Provider labels come from settings.
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

function renderJobsPager() {
  const { total, offset, has_more: hasMore } = state.history;
  $('#jobsPager').hidden = offset === 0 && !hasMore;
  $('#jobsPageStatus').textContent = state.jobs.length ? t('Showing {start}–{end} of {total}', {
    start: offset + 1, end: offset + state.jobs.length, total,
  }) : '';
  $('#jobsNewer').disabled = offset === 0;
  $('#jobsOlder').disabled = !hasMore;
}

function renderJobs() {
  const container = $('#jobs');
  renderJobsPager();
  if (!state.jobs.length) {
    container.innerHTML = `<div class="empty">${escapeHtml(t('No translations yet.'))}</div>`;
    return;
  }
  container.innerHTML = state.jobs.map(job => {
    const jobId = escapeHtml(encodeURIComponent(job.id));
    const targetNames = job.options.target_languages.map(languageName).join(', ');
    const owner = job.owner !== undefined ? ` · ${escapeHtml(job.owner || t('deleted user'))}` : '';
    const outputLinks = job.outputs.map(output =>
      `<a href="/api/jobs/${jobId}/download/${escapeHtml(encodeURIComponent(output.name))}">${escapeHtml(languageName(output.language))}</a>`
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
      <div class="job-meta">${escapeHtml(providerLabel(job.options.provider))} · ${escapeHtml(targetNames)}${owner}</div></div>
      <div><div class="progress-track"><div class="progress-bar" style="width:${Number(job.progress)}%"></div></div>
      <div class="job-meta">${escapeHtml(jobStatusText(job))} · ${Number(job.progress)}%</div>
      ${job.error ? `<div class="job-error">${escapeHtml(job.error)}</div>` : ''}
      ${job.warning ? `<div class="job-warning">${escapeHtml(job.warning)}</div>` : ''}</div>
      <div class="job-actions">${primaryAction}${cancelAction}${deleteAction}</div></article>`;
  }).join('');
}

function jobsUrl(params) {
  return `/api/jobs?${new URLSearchParams(params)}`;
}

function scheduleJobsPoll(delay) {
  clearTimeout(state.timer);
  state.timer = setTimeout(loadJobs, delay);
}

async function loadJobs() {
  if (!state.user) return; // Never poll while signed out.
  const session = state.session;
  const request = state.jobsRequest + 1;
  state.jobsRequest = request;
  state.jobsController?.abort(); // A newer load (scope toggle, page, refresh) wins.
  const controller = new AbortController();
  state.jobsController = controller;
  clearTimeout(state.timer);
  const current = () => session === state.session && request === state.jobsRequest;
  try {
    const admin = state.user.role === 'admin';
    const all = admin && $('#allJobs').checked;
    const historyParams = { limit: JOBS_PAGE_SIZE, offset: state.jobsOffset };
    if (all) historyParams.all = '1';
    // Administrators always see panel-wide dashboard metrics.
    const reuseHistory = state.jobsOffset === 0 && all === admin;
    const [history, overview] = await Promise.all([
      api(jobsUrl(historyParams), { signal: controller.signal }),
      reuseHistory ? null : api(jobsUrl({ limit: RECENT_JOBS, ...(admin ? { all: '1' } : {}) }),
        { signal: controller.signal }),
    ]);
    if (!current()) return;
    if (!history.jobs.length && history.offset > 0) {
      // The page emptied (deletions); step back to the last page that has jobs.
      state.jobsOffset = Math.max(0, Math.floor((history.total - 1) / JOBS_PAGE_SIZE) * JOBS_PAGE_SIZE);
      if (state.jobsOffset < history.offset) return loadJobs();
    }
    state.pollFailures = 0;
    state.jobsOffset = history.offset;
    state.jobs = history.jobs;
    state.history = history;
    state.overview = overview || history;
    renderJobs();
    renderDashboard();
    const active = activeJobCount(history.counts) > 0 || activeJobCount(state.overview.counts) > 0;
    scheduleJobsPoll(active ? POLL_ACTIVE_MS : POLL_IDLE_MS);
  } catch (error) {
    if (!current()) return; // Superseded, aborted, or from a previous session.
    if (error.status === 401) return showAuth(false);
    state.pollFailures += 1;
    if (state.pollFailures === 1) toast(error.message);
    scheduleJobsPoll(Math.min(POLL_IDLE_MS * 2 ** (state.pollFailures - 1), POLL_MAX_BACKOFF_MS));
  } finally {
    if (state.jobsController === controller) state.jobsController = null;
  }
}

function showJobsPage(offset) {
  state.jobsOffset = Math.max(0, offset);
  loadJobs();
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
    const saved = await sessionApi('/api/settings', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload),
    });
    form.querySelectorAll('input[type="password"]').forEach(input => { input.value = ''; });
    if (!saved) return;
    state.settings = saved;
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
  const data = await sessionApi('/api/users');
  if (!data) return;
  state.users = data.users;
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
$('#mfaLoginForm').addEventListener('submit', submitMfaLogin);
$('#mfaLoginBack').addEventListener('click', () => showAuth());
$('#mfaLoginResend').addEventListener('click', event => resendMfa('loginChallenge', event.currentTarget));
$('#mfaSetupResend').addEventListener('click', event => resendMfa('enrollmentChallenge', event.currentTarget));
$('#mfaMethod').addEventListener('change', updateMfaMethod);
$('#mfaSetupForm').addEventListener('submit', startMfaSetup);
$('#mfaConfirmForm').addEventListener('submit', confirmMfaSetup);
$('#mfaManageForm').addEventListener('submit', manageMfa);
$('#mfaManageEmail').addEventListener('click', sendManagementEmail);
$('#mfaSetupCancel').addEventListener('click', () => {
  clearMfaSecrets();
  loadMfa().catch(error => { $('#mfaSecurityError').textContent = error.message; });
});
$('#mfaSavedRecovery').addEventListener('click', () => {
  $('#mfaRecoveryCodes').textContent = '';
  $('#mfaRecovery').hidden = true;
});
$('#mfaDownloadRecovery').addEventListener('click', () => {
  const content = `Subtitle Translator — ${state.user.username}\n\n${$('#mfaRecoveryCodes').textContent}\n`;
  const url = URL.createObjectURL(new Blob([content], { type: 'text/plain;charset=utf-8' }));
  const link = document.createElement('a');
  link.href = url;
  link.download = 'subtitle-translator-recovery-codes.txt';
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
});
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
  if (state.user?.role !== 'admin' || !state.settings) return;
  resetSettingsForm();
  $('#settingsDialog').showModal();
}
$('#settingsButton').addEventListener('click', openSettings);
$('#providerSettingsButton').addEventListener('click', openSettings);
$('#closeSettings').addEventListener('click', () => $('#settingsDialog').close());
// Covers the close button, Escape, and the post-save close: never keep typed secrets.
$('#settingsDialog').addEventListener('close', clearSettingsSecrets);
$('#createUserForm').addEventListener('submit', createUser);
$('#userList').addEventListener('click', userAction);
$('#userList').addEventListener('change', userAction);
$('#refreshButton').addEventListener('click', () => loadJobs());
$('#jobsNewer').addEventListener('click', () => showJobsPage(state.jobsOffset - JOBS_PAGE_SIZE));
$('#jobsOlder').addEventListener('click', () => showJobsPage(state.jobsOffset + JOBS_PAGE_SIZE));
$('#dashboardRefresh').addEventListener('click', async () => {
  await Promise.all([loadJobs(), state.user?.role === 'admin' ? loadUsers() : Promise.resolve()]);
});
$('#allJobs').addEventListener('change', () => showJobsPage(0));
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
