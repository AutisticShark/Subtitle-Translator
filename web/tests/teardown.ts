import { rm } from 'node:fs/promises';
import { basename, dirname, resolve } from 'node:path';

export default async function teardown() {
  const path = process.env.E2E_DATA_DIR;
  if (
    !process.env.TEST_BASE_URL &&
    path &&
    dirname(path) === resolve('.') &&
    basename(path).startsWith('.e2e-')
  ) {
    await rm(path, { recursive: true, force: true });
  }
}
