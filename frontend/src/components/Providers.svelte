<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import ProviderRow from "./ProviderRow.svelte";

  // A provider added on this page opens for editing.
  let added = $state<string[]>([]);

  async function add() {
    const before = new Set((settings.draft.backends ?? []).map((b) => b.id));
    await settings.addProvider();
    const id = (settings.draft.backends ?? []).find((b) => !before.has(b.id))?.id;
    if (id) added = [...added, id];
  }
</script>

<Section title="Providers">
  {#each settings.draft.backends ?? [] as _, i (settings.draft.backends![i].id)}
    <ProviderRow bind:backend={settings.draft.backends![i]} open={added.includes(settings.draft.backends![i].id)} />
  {/each}
  <div class="add">
    <button class="link" onclick={add}>+ Add provider</button>
  </div>
  {#snippet footer()}
    {#if (settings.draft.backends ?? []).length === 0}
      A vLLM, SGLang or Flare server (OpenAI-compatible), or an Anthropic endpoint.
    {/if}
  {/snippet}
</Section>

<style>
  .add {
    padding: 9px 12px;
  }
  .add button {
    color: var(--accent);
  }
</style>
