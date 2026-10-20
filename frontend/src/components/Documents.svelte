<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import type { CheckResult } from "../lib/api";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import Note from "./Note.svelte";

  let result = $state<CheckResult | null>(null);
  let testing = $state(false);
  const value = $derived(settings.documentsValue);

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
  <Row label="Read documents" hint="PDFs and scans in a conversation">
    <select value={value} aria-label="Read documents"
      onchange={(e) => { result = null; settings.setDocuments(e.currentTarget.value); }}>
      {#each settings.documentsOptions as o (o.value)}
        <option value={o.value}>{o.label}</option>
      {/each}
    </select>
  </Row>
  {#if value === "paddle"}
    <div>
      <Row label="PaddleOCR URL" hint="Its /layout-parsing address" wide>
        <input type="url" bind:value={settings.draft.documents.paddleOCR} oninput={() => (result = null)}
          placeholder="http://host:8002/layout-parsing" spellcheck="false" aria-label="PaddleOCR URL" />
        <button onclick={test} disabled={testing || !settings.draft.documents.paddleOCR}>{testing ? "Testing…" : "Test"}</button>
      </Row>
      {#if result}
        <div class="pad"><Note level={result.ok ? "ok" : "error"} text={result.detail} /></div>
      {/if}
    </div>
  {/if}
  <Row label={value === "local" ? "On this computer" : "If that fails"}>
    <select bind:value={settings.draft.documents.local} aria-label="Read on this computer">
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
