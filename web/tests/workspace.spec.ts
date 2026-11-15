import { test, expect, type Page } from '@playwright/test';
import { createHmac } from 'node:crypto';

const username = 'workspace-admin';
const password = 'A-test-password-2026!';
const subtitle = '1\r\n00:00:01,000 --> 00:00:04,000\r\n<i>Hello there.</i>\r\n\r\n';
const vtt =
  'WEBVTT - captions\r\nKind: captions\r\n\r\nNOTE Keep this block\r\n\r\nintro\r\n00:01.000 --> 00:04.000 align:start\r\n<b>Tom &amp; Jerry</b>\r\n\r\n';

async function signIn(page: Page, name = username, pass = password) {
  await page.goto('/');
  await page.getByLabel('Username', { exact: true }).fill(name);
  await page.getByLabel('Password', { exact: true }).fill(pass);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Translate', exact: true })).toBeVisible();
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

test.describe.serial('Go server and Svelte workspace', () => {
  test('first run creates the administrator and configures Echo', async ({ page }) => {
    await page.goto('/');
    await expect(
      page.getByRole('heading', { name: 'Create administrator', exact: true }),
    ).toBeVisible();
    await page.getByLabel('Username', { exact: true }).fill(username);
    await page.getByLabel('Password', { exact: true }).fill(password);
    await page.getByLabel('Confirm password', { exact: true }).fill(password);
    await page.getByRole('button', { name: 'Create administrator', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Translate', exact: true })).toBeVisible();
    await page.getByRole('link', { name: 'Settings', exact: true }).click();
    await page.getByLabel('Default provider', { exact: true }).selectOption('echo');
    await page.getByRole('button', { name: 'Save changes', exact: true }).first().click();
    await expect(page.getByRole('status')).toContainText('Settings saved');
    const cookies = await page.context().cookies();
    expect(cookies.find((cookie) => cookie.name === 'access_token_cookie')?.httpOnly).toBe(true);
    expect(cookies.find((cookie) => cookie.name === 'access_token_cookie')?.sameSite).toBe(
      'Strict',
    );
    expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
    expect(await page.evaluate(() => Object.keys(sessionStorage))).toEqual([]);
  });

  test('uploads Unicode files, completes batches, and downloads preserved subtitles', async ({
    page,
  }) => {
    await signIn(page);
    await page.getByLabel('Choose subtitle files').setInputFiles([
      { name: '字幕第一集.SRT', mimeType: 'application/x-subrip', buffer: Buffer.from(subtitle) },
      { name: '字幕第二集.vtt', mimeType: 'text/vtt', buffer: Buffer.from(vtt) },
    ]);
    await expect(page.locator('.file-list li')).toHaveCount(2);
    await page.getByRole('button', { name: 'Translate', exact: true }).click();
    await expect(page.locator('.job-row')).toHaveCount(2);
    await expect(page.locator('.job-row').filter({ hasText: 'completed' })).toHaveCount(2);
    const first = page.locator('.job-row').filter({ hasText: '字幕第一集.SRT' });
    await first.locator('summary').click();
    const downloadEvent = page.waitForEvent('download');
    await first.locator('.download-options a').nth(1).click();
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
    const second = page.locator('.job-row').filter({ hasText: '字幕第二集.vtt' });
    const id = (await second.locator('.download-options a').first().getAttribute('href'))?.split(
      '/',
    )[3];
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
  });

  test('history survives polling failures and pauses in hidden tabs', async ({ page }) => {
    await signIn(page);
    await page.getByRole('link', { name: 'History', exact: true }).click();
    await expect(page.locator('.job-row')).toHaveCount(2);
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
    await expect(page.getByRole('alert')).toContainText('temporary test outage');
    await expect(page.getByRole('alert')).toHaveCount(0);
    expect(attempts).toBeGreaterThan(1);
    await expect(page.locator('.job-row')).toHaveCount(2);
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

  test('administrator creates a user whose job and settings access stays scoped', async ({
    page,
    browser,
  }) => {
    await signIn(page);
    await page.getByRole('link', { name: 'Users', exact: true }).click();
    await page.getByRole('button', { name: 'Create user', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel('Username', { exact: true }).fill('regular-user');
    await dialog.getByLabel('Password', { exact: true }).fill(password);
    await dialog.getByRole('button', { name: 'Create user', exact: true }).click();
    await expect(dialog).not.toBeVisible();
    await expect(
      page.locator('.user-name').getByText('regular-user', { exact: true }),
    ).toBeVisible();
    const context = await browser.newContext({ baseURL: test.info().project.use.baseURL });
    const regular = await context.newPage();
    await signIn(regular, 'regular-user');
    await expect(regular.getByRole('link', { name: 'Settings', exact: true })).toHaveCount(0);
    await expect(regular.locator('.job-row')).toHaveCount(0);
    const list = await (await page.request.get('/api/jobs')).json();
    expect((await regular.request.get(`/api/jobs/${list.jobs[0].id}`)).status()).toBe(404);
    expect(
      (await write(regular, '/api/settings', { default_provider: 'echo' }, 'PUT')).status(),
    ).toBe(403);
    expect((await regular.request.get('/api/users')).status()).toBe(403);
    await context.close();
  });

  test('light, dark, localized and narrow layouts stay usable', async ({ page }) => {
    await signIn(page);
    await page.getByLabel('Appearance', { exact: true }).selectOption('dark');
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await noOverflow(page);
    await page.screenshot({ path: 'test-results/workspace-dark.png', fullPage: true });
    await page.getByLabel('Appearance', { exact: true }).selectOption('light');
    await page.setViewportSize({ width: 375, height: 812 });
    await noOverflow(page);
    await page.screenshot({ path: 'test-results/workspace-mobile.png', fullPage: true });
    await page.getByRole('link', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Limits', exact: true }).click();
    await noOverflow(page);
    await page.getByRole('link', { name: 'Account', exact: true }).click();
    await noOverflow(page);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.getByRole('link', { name: 'Translate', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Translate', exact: true })).toBeVisible();
    await page.screenshot({ path: 'test-results/workspace-light.png', fullPage: true });
    await page.getByLabel('Interface language', { exact: true }).selectOption('zh-TW');
    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-TW');
    await noOverflow(page);
    await page.screenshot({ path: 'test-results/workspace-zh-TW.png', fullPage: true });
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
    await page.getByRole('button', { name: 'Sign out', exact: true }).last().click();
    await expect(page.getByRole('heading', { name: 'Translate', exact: true })).toBeVisible();
    await expect(page.getByRole('status')).toContainText('try logout again');
    await page.unroute('**/api/auth/logout');
    await page.getByRole('button', { name: 'Sign out', exact: true }).last().click();
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
    await page.getByRole('button', { name: 'Sign out', exact: true }).last().click();
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

  test('authenticator enrollment shows one-time recovery codes and supports recovery login', async ({
    page,
  }) => {
    await signIn(page);
    await page.getByRole('link', { name: 'Account', exact: true }).click();
    await page.getByLabel('Current password', { exact: true }).fill(password);
    await page.getByRole('button', { name: 'Set up two-step verification', exact: true }).click();
    await expect(page.getByLabel('Setup key', { exact: true })).toBeVisible();
    const secret = await page.getByLabel('Setup key', { exact: true }).inputValue();
    await expect(page.getByAltText('Authenticator setup QR code')).toBeVisible();
    await expect(page.getByLabel('Verification code', { exact: true })).toHaveAttribute(
      'pattern',
      '[0-9]{6}',
    );
    await page.getByLabel('Verification code', { exact: true }).fill(totp(secret));
    await page.getByRole('button', { name: 'Enable two-step verification', exact: true }).click();
    await expect(page.locator('.recovery-codes li')).toHaveCount(10);
    const recovery = await page.locator('.recovery-codes li').allTextContents();
    await page.getByRole('button', { name: 'I have saved my codes', exact: true }).click();
    await expect(page.locator('.recovery-codes')).toHaveCount(0);
    await page.getByRole('button', { name: 'Sign out', exact: true }).last().click();
    await page.getByLabel('Username', { exact: true }).fill(username);
    await page.getByLabel('Password', { exact: true }).fill(password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(
      page.getByRole('heading', { name: 'Two-step verification', exact: true }),
    ).toBeVisible();
    await page.getByLabel('Verification or recovery code', { exact: true }).fill(recovery[0]);
    await page.getByRole('button', { name: 'Verify and sign in', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Translate', exact: true })).toBeVisible();
    await page.getByRole('link', { name: 'Account', exact: true }).click();
    await page.getByLabel('Current password', { exact: true }).fill(password);
    await page.getByLabel('Verification or recovery code', { exact: true }).fill(recovery[1]);
    await page.getByRole('button', { name: 'Disable two-step verification', exact: true }).click();
    await expect(
      page.getByRole('button', { name: 'Set up two-step verification', exact: true }),
    ).toBeVisible();
  });
});
