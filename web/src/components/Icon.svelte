<script lang="ts">
  let {
    name,
    size = 18,
    stroke = 1.7,
  }: { name: string; size?: number; stroke?: number } = $props();
  const paths: Record<string, string> = {
    translate: 'M4 5h12M10 3v2M6 5c0 5 3 9 7 11M14 5c-1 5-4 9-10 12M14 21l4-10 4 10m-6-4h4',
    history: 'M3 11a9 9 0 1 1 2.7 7M3 4v7h7M12 7v5l3 2',
    settings:
      'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8M9 3l-.7 2.2-2 .8-2-.7-2 3.4L4 10.2v2l-1.7 1.5 2 3.4 2-.7 2 .8L9 21h4l.7-2.2 2-.8 2 .7 2-3.4-1.7-1.5v-2l1.7-1.5-2-3.4-2 .7-2-.8L13 3Z',
    users:
      'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 3a4 4 0 1 0 0 8 4 4 0 0 0 0-8M17 4a4 4 0 0 1 0 7m5 10v-2a4 4 0 0 0-3-3.9',
    shield: 'M12 3 3 7v5c0 5 9 10 9 10s9-5 9-10V7ZM8 12l3 3 5-6',
    upload: 'M12 16V3m-5 5 5-5 5 5M4 15v5a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-5',
    file: 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8ZM14 2v6h6M8 13h8m-8 4h5',
    arrow: 'M5 12h14m-5-5 5 5-5 5',
    down: 'm6 9 6 6 6-6',
    close: 'm6 6 12 12M6 18 18 6',
    check: 'm5 12 4 4L19 6',
    download: 'M12 3v12m-5-5 5 5 5-5M4 16v4h16v-4',
    refresh: 'M20 7V3m0 4h-4m4 0a9 9 0 1 0 1 8',
    logout: 'M9 4H4v16h5m5-13 5 5-5 5m-6-5h11',
    sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5',
    moon: 'M20 15A9 9 0 0 1 9 4a9 9 0 1 0 11 11Z',
    menu: 'M4 6h16M4 12h16M4 18h16',
    plus: 'M12 5v14M5 12h14',
    trash: 'M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7',
    lock: 'M6 10V7a6 6 0 0 1 12 0v3M4 10h16v12H4Zm8 5v3',
    globe: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0ZM3 12h18M12 3a17 17 0 0 1 0 18 17 17 0 0 1 0-18Z',
    copy: 'M8 8h12v13H8ZM16 8V3H3v13h5',
    search: 'M16 16l5 5M18 10a8 8 0 1 1-16 0 8 8 0 0 1 16 0Z',
    alert: 'm12 3 10 18H2ZM12 9v4m0 4h.01',
  };
</script>

<svg
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width={stroke}
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"><path d={paths[name] || paths.file} /></svg
>
