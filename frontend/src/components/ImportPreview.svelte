<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";

  const p = $derived(settings.importPreview!);
</script>

<Section title="Import settings">
  <div class="body">
    <div class="list"><span class="secondary">Providers</span><span>{(p.providers ?? []).join(", ") || "None"}</span></div>
    <div class="list"><span class="secondary">Models</span><span>{(p.models ?? []).join(", ") || "None"}</span></div>
    <div class="list"><span class="secondary">Roles</span><span>{(p.roles ?? []).join(", ") || "Unchanged"}</span></div>
  </div>
  <div class="actions">
    <button onclick={() => (settings.importPreview = null)}>Cancel</button>
    <button class="primary" onclick={() => settings.confirmImport()}>Import</button>
  </div>
  {#snippet footer()}
    Added to what you have; names already in use get a number. Nothing is saved until you say so.
  {/snippet}
</Section>

<style>
  .body {
    padding: 9px 12px;
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .list {
    display: grid;
    grid-template-columns: 80px 1fr;
    gap: 10px;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding: 9px 12px;
  }
</style>
