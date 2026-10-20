<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import Note from "./Note.svelte";

  let keys = $state<Record<string, string>>({});
  let errors = $state<Record<string, string>>({});
  let busy = $state(false);
  const ready = $derived(settings.needs.every((n) => !n.required || (keys[n.id] ?? "").trim() !== ""));

  async function go() {
    busy = true;
    try {
      const k: Record<string, string> = {};
      for (const n of settings.needs) k[n.id] = keys[n.id] ?? "";
      errors = await settings.applyKeys(k, true);
      if (Object.keys(errors).length === 0) settings.needs = [];
    } finally {
      busy = false;
    }
  }
</script>

<Section title="API keys for the imported providers">
  {#each settings.needs as n (n.id)}
    <div class="key">
      <div class="row">
        <div class="label">
          <span>{n.id}</span>
          <span class="secondary hint">{n.required ? "Required" : "Optional"}</span>
        </div>
        <input type="password" bind:value={keys[n.id]} autocomplete="off" aria-label="API key for {n.id}"
          placeholder={n.required ? "Paste your API key" : "If the server needs one"} />
      </div>
      {#if errors[n.id]}<div class="pad"><Note level="error" text={errors[n.id]} /></div>{/if}
      {#if n.joinURL}
        <div class="pad"><button class="link" onclick={() => settings.openURL(n.joinURL)}>Get a key for {n.host}</button></div>
      {/if}
    </div>
  {/each}
  <div class="actions">
    <button onclick={() => (settings.needs = [])}>Later</button>
    <button class="primary" onclick={go} disabled={busy || !ready}>{busy ? "Connecting…" : "Continue"}</button>
  </div>
  {#snippet footer()}
    Continue tests the keys and saves. Later keeps the import unsaved, to review first.
  {/snippet}
</Section>

<style>
  .row {
    display: flex;
    align-items: center;
    gap: 16px;
    padding: 8px 12px;
  }
  .label {
    display: flex;
    flex-direction: column;
    min-width: 120px;
  }
  .hint {
    font-size: 0.9em;
  }
  .row input {
    flex: 1;
  }
  .pad {
    padding: 0 12px 8px;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding: 9px 12px;
  }
</style>
