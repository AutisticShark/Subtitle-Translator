import { defineConfig, devices } from '@playwright/test';
import { mkdtempSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { resolve } from 'node:path';

const external = process.env.TEST_BASE_URL;
const baseURL = external || 'http://127.0.0.1:8165';
if (!external && !process.env.E2E_DATA_DIR) {
  process.env.E2E_DATA_DIR = mkdtempSync(resolve('.e2e-'));
}

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 45000,
  expect: { timeout: 10000 },
  reporter: [['list']],
  globalTeardown: './tests/teardown.ts',
  use: { baseURL, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: external
    ? undefined
    : {
        command: process.env.TEST_BINARY || 'go run ../cmd/subtitle-translator',
        url: `${baseURL}/healthz`,
        reuseExistingServer: false,
        timeout: 120000,
        env: {
          LISTEN_ADDR: new URL(baseURL).host,
          DATA_DIR: process.env.E2E_DATA_DIR!,
          JWT_SECRET_KEY: randomBytes(48).toString('hex'),
          API_KEY_ENCRYPTION_KEY: '',
          DATABASE_URL: '',
          REDIS_URL: '',
          EMAIL_PROVIDER: 'none',
          ADMIN_PASSWORD: '',
          APP_PASSWORD: '',
          APP_DEBUG: '1',
          FLASK_DEBUG: '1',
          JWT_COOKIE_SECURE: '0',
          CAPTCHA_PROVIDER: 'none',
          REGISTRATION_ENABLED: '1',
          MAX_UPLOAD_MB: '5',
        },
      },
});
