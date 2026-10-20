<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import type { CheckResult } from "../lib/api";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import Note from "./Note.svelte";

  let result = $state<CheckResult | null>(null);
  let testing = $state(false);

  async function test() {
    testing = true;
    try {
      result = await settings.testOCR(settings.draft.documents.paddleOCR ?? "");
    } finally {
      testing = false;
    }
  }
</script>

<Section title="Documents">
  <div>
    <Row label="PaddleOCR" hint="A /layout-parsing URL, used first" wide>
      <input type="url" bind:value={settings.draft.documents.paddleOCR} oninput={() => (result = null)}
        placeholder="Optional" spellcheck="false" aria-label="PaddleOCR URL" />
      <button onclick={test} disabled={testing || !settings.draft.documents.paddleOCR}>{testing ? "Testing…" : "Test"}</button>
    </Row>
    {#if result}
      <div class="pad"><Note level={result.ok ? "ok" : "error"} text={result.detail} /></div>
    {/if}
  </div>
  <Row label="Otherwise">
    <select bind:value={settings.draft.documents.local} aria-label="Without PaddleOCR">
      <option value="ocr">Read on this computer</option>
      <option value="text">PDF text only</option>
      <option value="off">Don't read documents</option>
    </select>
  </Row>
</Section>

<style>
  .pad {
    padding: 0 12px 9px;
  }
</style>
