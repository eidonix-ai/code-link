<script lang="ts">
  // One page per window: Settings at "/", a tool's launch window at
  // "#/launch/<tool>", its install / update progress at "#/install/<tool>".
  import SettingsPage from "./pages/SettingsPage.svelte";
  import LaunchPage from "./pages/LaunchPage.svelte";
  import InstallPage from "./pages/InstallPage.svelte";
  import { Tool } from "./lib/api";

  document.documentElement.classList.add(navigator.userAgent.includes("Windows") ? "win" : "mac");

  const [, kind, name] = location.hash.replace(/^#\/?/, "/").split("/");
  const tool = name === Tool.Codex ? Tool.Codex : Tool.ClaudeCode;
</script>

{#if kind === "launch"}
  <LaunchPage {tool} />
{:else if kind === "install"}
  <InstallPage {tool} />
{:else}
  <SettingsPage />
{/if}
