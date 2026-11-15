<script lang="ts">
  import { t } from '../lib/appState.svelte';
  let { compact = false }: { compact?: boolean } = $props();
</script>

<a class="brand" href="#translate" aria-label={t('Subtitle Translator')}>
  <svg class="brand-mark" width="32" height="32" viewBox="0 0 32 32" fill="none" aria-hidden="true"
    ><rect width="32" height="32" rx="8" fill="currentColor" /><g
      stroke="var(--brand-ink)"
      stroke-width="1.8"
      stroke-linecap="round"><path d="M10 9H7v14h3m12-14h3v14h-3M12 14h8m-8 5h5" /></g
    ></svg
  >
  {#if !compact}<span>Subtitle<span class="brand-second">Translator</span></span>{/if}
</a>
