<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import Switch from "./Switch.svelte";
  import { MenuAction } from "../lib/state.svelte";
  import type { ToolView } from "../lib/api";

  function line(t: ToolView): string {
    if (!t.checked) return "Checking…";
    if (!t.installed) return "Not installed";
    let s = t.version ? "Version " + t.version : "Installed";
    if (t.action === "update" && t.latest) s += " · " + t.latest + " available";
    return s;
  }
</script>

<Section title="Launch">
  {#each settings.tools as t (t.tool)}
    <Row label={t.name} hint={t.problem || line(t)}>
      {#if t.busy}
        <span class="secondary">Working…</span>
      {:else if t.action === "install"}
        <button onclick={() => settings.install(t.tool, MenuAction.ActInstall)}>Install</button>
      {:else if t.action === "update"}
        <button onclick={() => settings.install(t.tool, MenuAction.ActUpdate)}>Update</button>
      {/if}
    </Row>
  {/each}
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
