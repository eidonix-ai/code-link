<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import ModelSelect from "./ModelSelect.svelte";

  const none = $derived(settings.recommendation ? `The server's recommendation (${settings.recommendation})` : "The server's recommendation");
</script>

<Section title="Default model">
  {#if settings.groups.length === 0}
    <div class="pad secondary">The models appear here once a provider answers (Test it under Providers).</div>
  {:else}
    <Row label="Start with" hint="Each launch can pick another">
      <ModelSelect groups={settings.groups} value={settings.draft.launch.defaultModel ?? ""} {none} label="Default model"
        onchange={(id) => settings.setDefaultModel(id)} />
    </Row>
  {/if}
</Section>

<style>
  .pad {
    padding: 9px 12px;
  }
</style>
