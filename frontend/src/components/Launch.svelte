<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import Switch from "./Switch.svelte";
</script>

<Section title="Launch">
  <Row label="Terminal">
    <select bind:value={settings.draft.launch.terminal} aria-label="Terminal">
      {#each settings.platform.terminals ?? [] as t}
        <option value={t.value}>{t.label}</option>
      {/each}
    </select>
  </Row>
  <Row label="Claude Code permissions">
    <select value={settings.draft.launch.permissionMode || "auto"} aria-label="Claude Code permission mode"
      onchange={(e) => (settings.draft.launch.permissionMode = e.currentTarget.value === "auto" ? "" : e.currentTarget.value)}>
      {#each settings.platform.permissionModes ?? [] as m}
        <option value={m.value}>{m.label}</option>
      {/each}
    </select>
  </Row>
  <Row label="Port" hint="Claude Code and Codex connect to 127.0.0.1 on this port">
    <input class="port" type="number" min="1024" max="65535" bind:value={settings.draft.port} aria-label="Port" />
  </Row>
  <Row label="Open at login">
    <Switch label="Open at login" bind:checked={settings.draft.launch.openAtLogin} />
  </Row>
</Section>

<style>
  .port {
    width: 84px;
    text-align: right;
  }
</style>
