<script lang="ts">
  import { settings, Tool } from "../lib/state.svelte";

  const where = $derived(settings.platform.os === "windows"
    ? "code-link lives in the system tray, by the clock: click its icon to launch again or to change settings."
    : "code-link lives in the menu bar: click its icon to launch again or to change settings.");
</script>

<div class="done">
  <h1>You're set up</h1>
  <p class="secondary">The models on {settings.draft.backends?.[0]?.id ?? "your provider"} are ready for Claude Code and Codex.</p>
  <button class="primary" onclick={() => settings.openLaunch(Tool.ClaudeCode)}>Launch Claude Code</button>
  <p class="secondary note">{where}</p>
  <div><button class="link" onclick={() => (settings.setUp = false)}>Open Settings</button></div>
</div>

<style>
  .done {
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
    margin: 0;
  }
  .primary {
    align-self: flex-start;
  }
  .note {
    font-size: 0.94em;
  }
</style>
