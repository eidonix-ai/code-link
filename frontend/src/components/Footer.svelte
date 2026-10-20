<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Dot from "./Dot.svelte";
</script>

<footer>
  <div class="status">
    {#if settings.message && (settings.message.level !== "ok" || !settings.dirty)}
      <Dot level={settings.message.level} />
      <span class="text" title={settings.message.text}>{settings.message.text}</span>
    {:else if settings.dirty}
      <span class="secondary">Unsaved changes</span>
    {:else}
      <Dot level={settings.status.level} />
      <span class="text secondary" title={settings.status.line}>{settings.status.line}</span>
    {/if}
  </div>
  <button onclick={() => settings.revert()} disabled={!settings.dirty || settings.saving}>Revert</button>
  <button class="primary" onclick={() => settings.save()} disabled={!settings.dirty || settings.saving}>Save</button>
</footer>

<style>
  footer {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 20px;
    border-top: 1px solid var(--separator);
    background: var(--footer);
    backdrop-filter: blur(20px);
  }
  .status {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 7px;
    min-width: 0;
  }
  .text {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
