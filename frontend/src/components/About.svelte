<script lang="ts">
  // code-link's own version, and checking for and installing an update.
  import { onMount } from "svelte";
  import { Settings, onUpdate, type UpdateState } from "../lib/api";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";

  let s = $state<UpdateState | null>(null);
  /** Set by "Check for updates", so only a check asked for shows its failure. */
  let asked = $state(false);

  onMount(() => {
    Settings.Update().then((u) => (s = u));
    return onUpdate((u) => (s = u));
  });

  async function check() {
    asked = true;
    s = await Settings.CheckForUpdates();
  }

  const hint = $derived.by(() => {
    if (!s) return "";
    if (s.disabled) return s.disabled;
    if (s.restartNeeded) return "Quit and reopen code-link to finish updating";
    if (s.job.running) return `Updating to ${s.job.version}…`;
    if (s.checking) return "Checking for updates…";
    if (s.available) return `Version ${s.latest} is available`;
    if (asked && s.error) return s.error;
    if (s.checkedAt) return "Up to date";
    return "";
  });
</script>

{#if s}
  <Section>
    <Row label={`code-link ${s.current}`} hint={hint}>
      {#if s.available && !s.job.running}
        <button class="primary" onclick={() => Settings.StartUpdate()}>Update to {s.latest}…</button>
      {:else if s.job.running}
        <button onclick={() => Settings.StartUpdate()}>Show progress</button>
      {/if}
      <button onclick={check} disabled={!!s.disabled || s.checking || s.job.running}>Check for updates</button>
    </Row>
  </Section>
{/if}
