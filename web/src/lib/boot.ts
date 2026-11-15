export interface BootData {
  locales: Record<string, string>;
  languages: string[];
  maxUploadMB: number;
}

// The server embeds the interface locales, the ordered language list and the
// upload limit in the initial HTML. Vite's dev server serves a bare shell, so
// every value has a fallback.
export function readBoot(): BootData {
  const fallback: BootData = {
    locales: { en: 'English', 'zh-TW': '正體中文', 'zh-CN': '简体中文' },
    languages: [],
    maxUploadMB: 0,
  };
  try {
    const boot = JSON.parse(document.getElementById('app-boot')?.textContent || '{}') as {
      locales?: { code: string; label: string }[];
      languages?: { code: string }[];
      max_upload_mb?: number;
    };
    if (Array.isArray(boot.locales) && boot.locales.length) {
      fallback.locales = Object.fromEntries(
        boot.locales
          .filter((item) => typeof item.code === 'string' && typeof item.label === 'string')
          .map((item) => [item.code, item.label]),
      );
    }
    if (Array.isArray(boot.languages)) {
      fallback.languages = boot.languages
        .map((item) => item.code)
        .filter((code) => typeof code === 'string');
    }
    if (typeof boot.max_upload_mb === 'number') fallback.maxUploadMB = boot.max_upload_mb;
  } catch {
    /* Malformed or missing bootstrap data keeps the defaults. */
  }
  return fallback;
}
