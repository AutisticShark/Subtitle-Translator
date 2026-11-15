import { test, expect, type Page } from '@playwright/test';
import { createHmac } from 'node:crypto';

const username = 'workspace-admin';
const password = 'A-test-password-2026!';
const subtitleFilename = '字幕第一集—行動裝置與桌面排版驗證用的超長正體中文字幕檔案名稱.SRT';
const subtitle = '1\r\n00:00:01,000 --> 00:00:04,000\r\n<i>Hello there.</i>\r\n\r\n';
const vtt =
  'WEBVTT - captions\r\nKind: captions\r\n\r\nNOTE Keep this block\r\n\r\nintro\r\n00:01.000 --> 00:04.000 align:start\r\n<b>Tom &amp; Jerry</b>\r\n\r\n';
const views = {
  dashboard: 'Dashboard',
  translate: 'New translation',
  jobs: 'Translation history',
  security: 'Account security',
  admin: 'Administration',
} as const;

async function navigate(page: Page, view: keyof typeof views) {
  await page
    .locator('.primary-nav')
    .getByRole('button', { name: views[view], exact: true })
    .click();
  await expect(page.locator('#viewTitle')).toHaveText(views[view]);
}
async function signIn(page: Page, name = username, pass = password) {
  await page.goto('/');
  await page.getByLabel('Username', { exact: true }).fill(name);
  await page.getByLabel('Password', { exact: true }).fill(pass);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.locator('#viewTitle')).toHaveText('Dashboard');
}
async function openSettings(page: Page) {
  await navigate(page, 'admin');
  await page.getByRole('button', { name: /Configure panel/ }).click();
  await expect(page.locator('#settingsDialog')).toBeVisible();
}
async function write(page: Page, path: string, data: unknown, method = 'POST') {
  const csrf =
    (await page.context().cookies()).find((cookie) => cookie.name === 'csrf_access_token')?.value ||
    '';
  return page.request.fetch(path, { method, data, headers: { 'X-CSRF-TOKEN': csrf } });
}
function totp(secret: string) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const character of secret.toUpperCase().replace(/=+$/, ''))
    bits += alphabet.indexOf(character).toString(2).padStart(5, '0');
  const key = Buffer.from((bits.match(/.{8}/g) || []).map((byte) => Number.parseInt(byte, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const digest = createHmac('sha1', key).update(counter).digest();
  const offset = digest[digest.length - 1] & 15;
  return ((digest.readUInt32BE(offset) & 0x7fffffff) % 1000000).toString().padStart(6, '0');
}
async function noOverflow(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  );
}
async function accent(page: Page) {
  return page
    .locator('html')
    .evaluate((node) => getComputedStyle(node).getPropertyValue('--accent').trim());
}

test.describe.serial('Original interface on the Go and Svelte stack', () => {
  test('first run retains the original shell, dashboard and panel settings workflow', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto('/');
    await expect(
      page.getByRole('heading', { name: 'Create the first administrator', exact: true }),
    ).toBeVisible();
    await expect(page.locator('.auth-panel .brand-mark')).toHaveText('文');
    await page.getByLabel('Username', { exact: true }).fill(username);
    await page.getByLabel('Password', { exact: true }).fill(password);
    const confirmation = page.getByLabel('Confirm password', { exact: true });
    if (await confirmation.isVisible()) await confirmation.fill(password);
    await page.getByRole('button', { name: 'Create administrator', exact: true }).click();
    await expect(page.locator('#viewTitle')).toHaveText('Dashboard');
    await expect(page.locator('.primary-nav .nav-item')).toHaveCount(5);
    for (const label of Object.values(views)) {
      await expect(
        page.locator('.primary-nav').getByRole('button', { name: label, exact: true }),
      ).toBeVisible();
    }
    await expect(page.locator('#dashboardMetrics .metric-card')).toHaveCount(4);
    await expect(page.locator('.quick-card')).toBeVisible();
    await expect(page.locator('.topbar .theme-picker')).toBeVisible();
    await expect(page.locator('.locale-picker')).toBeVisible();
    expect(
      await page
        .locator('.sidebar')
        .evaluate((node) => Math.round(node.getBoundingClientRect().width)),
    ).toBe(248);
    expect(
      await page.locator('#viewTitle').evaluate((node) => getComputedStyle(node).fontFamily),
    ).toContain('Georgia');
    await page.getByLabel('Appearance', { exact: true }).selectOption('light');
    expect(await accent(page)).toBe('#4f7418');
    await openSettings(page);
    const dialog = page.locator('#settingsDialog');
    await dialog
      .getByRole('combobox', { name: 'Default provider', exact: true })
      .selectOption('echo');
    const saved = page.waitForResponse(
      (response) =>
        response.url().endsWith('/api/settings') && response.request().method() === 'PUT',
    );
    await dialog.getByRole('button', { name: 'Save settings', exact: true }).click();
    expect((await saved).ok()).toBe(true);
    await expect(dialog).not.toBeVisible();
    const settings = await (await page.request.get('/api/settings')).json();
    expect(settings.default_provider).toBe('echo');
    const cookies = await page.context().cookies();
    expect(cookies.find((cookie) => cookie.name === 'access_token_cookie')?.httpOnly).toBe(true);
    expect(cookies.find((cookie) => cookie.name === 'access_token_cookie')?.sameSite).toBe(
      'Strict',
    );
    expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
    expect(await page.evaluate(() => Object.keys(sessionStorage))).toEqual([]);
  });

  test('original upload form shows target chips and downloads preserved Unicode subtitles', async ({
    page,
  }) => {
    await signIn(page);
    await navigate(page, 'translate');
    await expect(page.locator('#translateForm .dropzone')).toBeVisible();
    const chips = page.locator('#languagePicker .chip');
    expect(await chips.count()).toBeGreaterThan(10);
    for (const chip of await chips.all()) await expect(chip).toBeVisible();
    await page.getByRole('combobox', { name: 'Provider', exact: true }).selectOption('anthropic');
    await expect(page.getByLabel(/Model override/)).toBeVisible();
    await page.getByRole('combobox', { name: 'Provider', exact: true }).selectOption('echo');
    await page.locator('#fileInput').setInputFiles([
      { name: subtitleFilename, mimeType: 'application/x-subrip', buffer: Buffer.from(subtitle) },
      { name: '字幕第二集.vtt', mimeType: 'text/vtt', buffer: Buffer.from(vtt) },
    ]);
    await expect(page.locator('.file-pill')).toHaveCount(2);
    await page.getByRole('button', { name: /Start translating/ }).click();
    await expect(page.locator('.file-pill')).toHaveCount(0);
    await navigate(page, 'jobs');
    await expect(page.locator('.job')).toHaveCount(2);
    await expect(page.locator('.job .download-actions')).toHaveCount(2);
    const first = page.locator('.job').filter({ hasText: subtitleFilename });
    await expect(first.locator('.language-downloads a')).toHaveCount(2);
    const downloadEvent = page.waitForEvent('download');
    await first.locator('.language-downloads a').first().click();
    const download = await downloadEvent;
    const stream = await download.createReadStream();
    const chunks: Buffer[] = [];
    if (stream) for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    const text = Buffer.concat(chunks).toString('utf8');
    expect(text).toContain('00:00:01,000 --> 00:00:04,000');
    expect(text).toContain('<i>');
    expect(text).toContain('</i>');
    expect(text).toContain('\r\n');
    expect(text).not.toContain('\r\r\n');
    const second = page.locator('.job').filter({ hasText: '字幕第二集.vtt' });
    const id = (await second.locator('.download').getAttribute('href'))?.split('/')[3];
    expect(id).toBeTruthy();
    const detail = await (await page.request.get(`/api/jobs/${id}`)).json();
    const response = await page.request.get(
      `/api/jobs/${id}/download/${encodeURIComponent(detail.outputs[0].name)}`,
    );
    const result = await response.text();
    expect(result).toContain('WEBVTT - captions\r\nKind: captions');
    expect(result).toContain('NOTE Keep this block');
    expect(result).toContain('align:start');
    expect(result).toContain('<b>');
    expect(result).toContain('&amp;');
    await navigate(page, 'dashboard');
    await expect(page.locator('#dashboardMetrics .metric-value')).toHaveText(['2', '0', '2', '0']);
    await expect(page.locator('#dashboardRecent .recent-row')).toHaveCount(2);
  });

  test('history survives polling failures and pauses in hidden tabs', async ({ page }) => {
    await signIn(page);
    await navigate(page, 'jobs');
    await expect(page.locator('.job')).toHaveCount(2);
    let attempts = 0;
    await page.route('**/api/jobs?*', async (route) => {
      if (++attempts === 1)
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: '{"error":"temporary test outage"}',
        });
      else await route.continue();
    });
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await expect(page.locator('.toast.show')).toContainText('temporary test outage');
    await expect.poll(() => attempts, { timeout: 15000 }).toBeGreaterThan(1);
    await expect(page.locator('.toast')).not.toHaveClass(/show/);
    await expect(page.locator('.job')).toHaveCount(2);
    await page.unroute('**/api/jobs?*');
    let requests = 0;
    page.on('request', (request) => {
      if (request.url().includes('/api/jobs?')) requests++;
    });
    await page.evaluate(() => {
      Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
      document.dispatchEvent(new Event('visibilitychange'));
    });
    const before = requests;
    await page.waitForTimeout(11000);
    expect(requests).toBe(before);
    await page.evaluate(() => {
      Object.defineProperty(document, 'hidden', { configurable: true, get: () => false });
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await expect.poll(() => requests).toBeGreaterThan(before);
  });

  test('administration restores inline user creation and scoped dashboards', async ({
    page,
    browser,
  }) => {
    await signIn(page);
    await navigate(page, 'admin');
    await expect(page.locator('#adminMetrics .metric-card')).toHaveCount(4);
    await expect(page.locator('#adminProviders .provider-admin-row')).not.toHaveCount(0);
    const form = page.locator('#createUserForm');
    await form.getByLabel('Username', { exact: true }).fill('regular-user');
    await form.getByLabel('Temporary password', { exact: true }).fill(password);
    await form.getByRole('button', { name: 'Create user', exact: true }).click();
    await expect(
      page.locator('#userList .user-row').filter({ hasText: 'regular-user' }),
    ).toBeVisible();
    const context = await browser.newContext({ baseURL: test.info().project.use.baseURL });
    const regular = await context.newPage();
    await signIn(regular, 'regular-user');
    await expect(regular.locator('.primary-nav .nav-item')).toHaveCount(4);
    await expect(
      regular.locator('.primary-nav').getByRole('button', { name: 'Administration', exact: true }),
    ).toHaveCount(0);
    await expect(regular.locator('#dashboardMetrics .metric-value')).toHaveText([
      '0',
      '0',
      '0',
      '0',
    ]);
    await navigate(regular, 'jobs');
    await expect(regular.locator('.job')).toHaveCount(0);
    const list = await (await page.request.get('/api/jobs')).json();
    expect((await regular.request.get(`/api/jobs/${list.jobs[0].id}`)).status()).toBe(404);
    expect(
      (await write(regular, '/api/settings', { default_provider: 'echo' }, 'PUT')).status(),
    ).toBe(403);
    expect((await regular.request.get('/api/users')).status()).toBe(403);
    await context.close();
  });

  test('panel settings clear unsaved keys and support keyboard dismissal', async ({ page }) => {
    await signIn(page);
    await openSettings(page);
    const dialog = page.locator('#settingsDialog');
    await dialog.getByLabel(/^Anthropic API key/).fill('offline-test-only-not-a-real-key');
    await page.keyboard.press('Escape');
    await expect(dialog).not.toBeVisible();
    await page.getByRole('button', { name: /Configure panel/ }).click();
    await expect(dialog.getByLabel(/^Anthropic API key/)).toHaveValue('');
    expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
    expect(await page.evaluate(() => Object.keys(sessionStorage))).toEqual([]);
    await dialog.getByRole('button', { name: 'Close', exact: true }).click();
    const settings = await (await page.request.get('/api/settings')).json();
    expect(settings.configured.anthropic).toBe(false);
    expect(JSON.stringify(settings)).not.toContain('offline-test-only');
  });

  test('leaving administration closes dialogs and clears typed secrets', async ({ page }) => {
    await signIn(page);
    await openSettings(page);
    await page
      .locator('#settingsDialog input[name="anthropic_api_key"]')
      .fill('offline-unsaved-only');
    await page.evaluate(() => {
      window.location.hash = '#translate';
    });
    await expect(page.locator('#viewTitle')).toHaveText('New translation');
    await expect(page.locator('dialog:modal')).toHaveCount(0);
    await page.getByLabel('Source language', { exact: true }).focus();
    await expect(page.getByLabel('Source language', { exact: true })).toBeFocused();
    await openSettings(page);
    await expect(page.locator('#settingsDialog input[name="anthropic_api_key"]')).toHaveValue('');
    await page.keyboard.press('Escape');
    await page
      .locator('#userList .user-row')
      .filter({ hasText: 'regular-user' })
      .getByRole('button', { name: 'Reset password', exact: true })
      .click();
    await page
      .locator('#resetPasswordDialog input[type="password"]')
      .fill('Offline-unsaved-password!');
    await page.evaluate(() => {
      window.location.hash = '#jobs';
    });
    await expect(page.locator('#viewTitle')).toHaveText('Translation history');
    await expect(page.locator('dialog:modal')).toHaveCount(0);
    await expect(page.locator('#resetPasswordDialog input[type="password"]')).toHaveValue('');
  });

  test('a rejected appearance save restores the saved theme and selector', async ({ page }) => {
    await signIn(page);
    const appearance = page.getByRole('combobox', { name: 'Appearance', exact: true });
    const previous = await appearance.inputValue();
    await page.route('**/api/auth/me', async (route) => {
      if (route.request().method() === 'PATCH')
        await route.fulfill({
          status: 503,
          contentType: 'application/json',
          body: '{"error":"appearance test outage"}',
        });
      else await route.continue();
    });
    await appearance.selectOption(previous === 'dark' ? 'light' : 'dark');
    await expect(page.locator('.toast.show')).toContainText('appearance test outage');
    await expect(appearance).toHaveValue(previous);
    await expect(page.locator('html')).toHaveAttribute('data-theme', previous);
  });

  test('original light, dark, localized and narrow layouts stay usable', async ({ page }) => {
    await signIn(page);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.getByLabel('Appearance', { exact: true }).selectOption('dark');
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    expect(await accent(page)).toBe('#c7ff5e');
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await expect(page.locator('#viewTitle')).toHaveText('Dashboard');
    await noOverflow(page);
    await page.screenshot({ path: 'test-results/dashboard-dark.png', fullPage: true });
    await navigate(page, 'translate');
    await page.screenshot({ path: 'test-results/translation-dark.png', fullPage: true });
    await page.getByLabel('Appearance', { exact: true }).selectOption('light');
    await page.screenshot({ path: 'test-results/translation-light.png', fullPage: true });
    await page.setViewportSize({ width: 375, height: 812 });
    await noOverflow(page);
    await page.screenshot({ path: 'test-results/translation-mobile.png', fullPage: true });
    for (const view of ['dashboard', 'jobs', 'security', 'admin'] as const) {
      await navigate(page, view);
      await noOverflow(page);
    }
    await page.screenshot({ path: 'test-results/administration-mobile.png', fullPage: true });
    await page.getByRole('button', { name: /Configure panel/ }).click();
    const dialog = page.locator('#settingsDialog');
    await expect(dialog).toBeVisible();
    expect(await dialog.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true);
    await dialog.getByText('Submission rate limits', { exact: true }).click();
    expect(await dialog.evaluate((node) => node.scrollWidth <= node.clientWidth)).toBe(true);
    await page.screenshot({ path: 'test-results/settings-mobile.png', fullPage: true });
    await page.keyboard.press('Escape');
    await page.setViewportSize({ width: 1440, height: 1000 });
    await navigate(page, 'dashboard');
    await page.screenshot({ path: 'test-results/dashboard-light.png', fullPage: true });
    await page.getByLabel('Interface language', { exact: true }).selectOption('zh-TW');
    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-TW');
    await expect(page.locator('#viewTitle')).toBeVisible();
    await noOverflow(page);
    await page.screenshot({ path: 'test-results/dashboard-zh-TW.png', fullPage: true });
    await page.getByLabel('介面語言', { exact: true }).selectOption('en');
    await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  });

  test('failed logout keeps the live session visible', async ({ page }) => {
    await signIn(page);
    await page.route('**/api/auth/logout', (route) =>
      route.fulfill({
        status: 503,
        contentType: 'application/json',
        body: '{"error":"try logout again"}',
      }),
    );
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page.locator('#viewTitle')).toHaveText('Dashboard');
    await expect(page.locator('.toast')).toContainText('try logout again');
    await page.unroute('**/api/auth/logout');
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
  });

  test('a late theme response cannot restore a signed-out session', async ({ page }) => {
    await signIn(page);
    let release!: () => void;
    let received!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const ready = new Promise<void>((resolve) => {
      received = resolve;
    });
    await page.route('**/api/auth/me', async (route) => {
      if (route.request().method() !== 'PATCH') {
        await route.continue();
        return;
      }
      const response = await route.fetch();
      received();
      await gate;
      await route.fulfill({ response });
    });
    await page.getByLabel('Appearance', { exact: true }).selectOption('dark');
    await ready;
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    const lateResponse = page.waitForResponse(
      (response) =>
        response.url().endsWith('/api/auth/me') && response.request().method() === 'PATCH',
    );
    release();
    await lateResponse;
    await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
    await expect(page.getByRole('navigation', { name: 'Main navigation' })).toHaveCount(0);
  });

  test('authenticator enrollment, recovery login and factor management retain their original flow', async ({
    page,
  }) => {
    await signIn(page);
    await navigate(page, 'security');
    await page.getByLabel('Current password', { exact: true }).fill(password);
    await page.getByRole('button', { name: 'Start MFA setup', exact: true }).click();
    await expect(page.getByLabel('Setup key', { exact: true })).toBeVisible();
    const secret = await page.getByLabel('Setup key', { exact: true }).inputValue();
    await expect(page.getByAltText('Authenticator setup QR code')).toBeVisible();
    await expect(page.getByLabel('Verification code', { exact: true })).toHaveAttribute(
      'pattern',
      '[0-9]{6}',
    );
    await page.getByLabel('Verification code', { exact: true }).fill(totp(secret));
    await page.getByRole('button', { name: 'Verify and enable MFA', exact: true }).click();
    await expect(page.locator('#mfaRecoveryCodes')).toBeVisible();
    const recovery = (await page.locator('#mfaRecoveryCodes').textContent())!.trim().split(/\s+/);
    expect(recovery).toHaveLength(10);
    await page.getByRole('button', { name: 'I have saved my codes', exact: true }).click();
    await expect(page.locator('#mfaRecoveryCodes')).not.toBeVisible();
    await page.getByRole('button', { name: 'Sign out', exact: true }).click();
    await page.getByLabel('Username', { exact: true }).fill(username);
    await page.getByLabel('Password', { exact: true }).fill(password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(
      page.getByRole('heading', { name: 'Multi-factor authentication', exact: true }),
    ).toBeVisible();
    await page.getByLabel('Verification or recovery code', { exact: true }).fill(recovery[0]);
    await page.getByRole('button', { name: 'Verify and sign in', exact: true }).click();
    await expect(page.locator('#viewTitle')).toHaveText('Account security');
    await navigate(page, 'security');
    await page.getByLabel('Current password', { exact: true }).fill(password);
    await page.getByLabel('Verification or recovery code', { exact: true }).fill(recovery[1]);
    await page.getByRole('button', { name: 'Disable MFA', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Start MFA setup', exact: true })).toBeVisible();
  });
});
