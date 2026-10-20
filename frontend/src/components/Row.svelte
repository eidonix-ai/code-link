<script lang="ts">
  import type { Snippet } from "svelte";
  let { label, hint, children, wide = false }: { label: string; hint?: string; children: Snippet; wide?: boolean } = $props();
</script>

<div class="row" class:wide>
  <div class="label">
    <span>{label}</span>
    {#if hint}<span class="hint">{hint}</span>{/if}
  </div>
  <div class="control">{@render children()}</div>
</div>

<style>
  .row {
    display: flex;
    align-items: center;
    gap: 16px;
    padding: 8px 12px;
    min-height: 40px;
  }
  .label {
    flex: 0 0 auto;
    display: flex;
    flex-direction: column;
    max-width: 45%;
  }
  .hint {
    color: var(--secondary);
    font-size: 0.9em;
  }
  .control {
    flex: 1;
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .wide .control :global(input[type="text"]),
  .wide .control :global(input[type="url"]),
  .wide .control :global(input[type="password"]) {
    flex: 1;
  }
  .control :global(select) {
    max-width: 100%;
  }
</style>
