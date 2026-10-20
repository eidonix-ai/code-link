<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import type { CheckResult } from "../lib/api";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import Note from "./Note.svelte";

  let result = $state<CheckResult | null>(null);
  let testing = $state(false);
  const own = $derived(settings.searchValue === "tavily" || settings.searchValue === "brave");

  async function test() {
    testing = true;
    try {
      result = await settings.testSearch();
    } finally {
      testing = false;
    }
  }
</script>

<Section title="Web search">
  <Row label="Search">
    <select value={settings.searchValue} aria-label="Web search"
      onchange={(e) => { result = null; settings.setSearch(e.currentTarget.value); }}>
      {#each settings.searchOptions as o (o.value)}
        <option value={o.value}>{o.label}</option>
      {/each}
    </select>
  </Row>
  {#if own}
    <div>
      <Row label="API key" wide>
        <input type="password" bind:value={settings.draft.webSearch.apiKey} oninput={() => (result = null)}
          placeholder="Required" autocomplete="off" aria-label="Web search API key" />
        <button onclick={test} disabled={testing || !settings.draft.webSearch.apiKey}>{testing ? "Testing…" : "Test"}</button>
      </Row>
      {#if result}
        <div class="pad"><Note level={result.ok ? "ok" : "error"} text={result.detail} /></div>
      {/if}
    </div>
  {/if}
</Section>

<style>
  .pad {
    padding: 0 12px 9px;
  }
</style>
