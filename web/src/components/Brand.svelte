<script lang="ts">
  import { t } from '../lib/appState.svelte';
  let { href = '/' }: { href?: string } = $props();
</script>

<a class="brand" {href} aria-label={t('Subtitle Translator')}>
  <span class="brand-mark">文</span>
  <span>{t('Subtitle Translator')}<small>{t('Private workspace')}</small></span>
</a>
