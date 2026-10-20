<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import type { KeyNeed } from "../lib/api";
  import Note from "./Note.svelte";

  let { need }: { need: KeyNeed } = $props();
  let key = $state("");
  let error = $state("");
  let busy = $state(false);

  async function go() {
    if (busy || !key.trim()) return;
    busy = true;
    error = "";
    try {
      const errs = await settings.applyKeys({ [need.id]: key }, true);
      error = errs[need.id] ?? "";
    } catch (e) {
      error = String(e);
    } finally {
      busy = false;
    }
  }
</script>

<div class="welcome">
  <h1>Welcome to code-link</h1>
  <p class="secondary">Claude Code and Codex on the models at {need.host}. Paste your API key to start.</p>
  <label class="field">
    <span>Paste your API key</span>
    <!-- svelte-ignore a11y_autofocus -->
    <input type="password" bind:value={key} autocomplete="off" autofocus
      onkeydown={(e) => e.key === "Enter" && go()} oninput={() => (error = "")} />
  </label>
  {#if error}
    <Note level="error" text={error} />
  {:else if settings.message?.level === "error"}
    <Note level="error" text={settings.message.text} />
  {/if}
  <button class="primary continue" onclick={go} disabled={busy || !key.trim()}>{busy ? "Connecting…" : "Continue"}</button>
  <div class="links">
    {#if need.joinURL}
      <button class="link" onclick={() => settings.openURL(need.joinURL)}>Get a key</button>
      <span class="secondary">·</span>
    {/if}
    <button class="link" onclick={() => (settings.getStarted = null)}>Set up manually</button>
  </div>
</div>

<style>
  .welcome {
    max-width: 380px;
    margin: 0 auto;
    padding: 72px 20px 20px;
    display: flex;
    flex-direction: column;
    gap: 14px;
  }
  h1 {
    margin: 0;
    font-size: 1.55em;
    font-weight: 600;
  }
  p {
    margin: 0 0 6px;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .field input {
    width: 100%;
  }
  .continue {
    align-self: flex-start;
  }
  .links {
    display: flex;
    gap: 8px;
    margin-top: 4px;
  }
</style>
