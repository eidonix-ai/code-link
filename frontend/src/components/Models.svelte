<script lang="ts">
  import { settings, Slot } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
  import Note from "./Note.svelte";
  import ModelPicker from "./ModelPicker.svelte";

  let advanced = $state(false);
  const overrides = $derived(
    [settings.draft.roles.opus, settings.draft.roles.small, settings.draft.roles.decision, settings.draft.roles.vision, settings.draft.roles.codex]
      .filter((x) => x).length,
  );
  const hasBackends = $derived((settings.draft.backends ?? []).length > 0);
</script>

<Section title="Model">
  {#each settings.warnings as w}
    <div class="pad"><Note level="warn" text={w} /></div>
  {/each}
  {#if !hasBackends}
    <div class="pad secondary">Add a provider first.</div>
  {:else}
    <Row label="Main model" hint="Claude Code's default">
      <ModelPicker role={Slot.SlotMain} label="Main model" />
    </Row>
    <button class="disclosure" onclick={() => (advanced = !advanced)} aria-expanded={advanced}>
      <span class="chev" class:open={advanced}>›</span>
      <span>Advanced</span>
      <span class="spacer"></span>
      {#if overrides > 0 && !advanced}<span class="secondary">{overrides} set</span>{/if}
    </button>
    {#if advanced}
      <Row label="Opus" hint="Plan mode, /model opus and Fable">
        <ModelPicker role={Slot.SlotOpus} none="Same as main" label="Opus model" />
      </Row>
      <Row label="Haiku" hint="Quick background work: titles, summaries">
        <ModelPicker role={Slot.SlotSmall} none="Same as main" label="Haiku model" />
      </Row>
      <div class="stack">
        <Row label="Decision" hint="Approves actions in auto mode">
          <ModelPicker role={Slot.SlotDecision} none="Same as main" label="Decision model" />
        </Row>
        {#if settings.decision}
          <div class="note">{settings.decision}</div>
        {/if}
      </div>
      <Row label="Vision" hint="Describes images for a model that can't see them">
        <ModelPicker role={Slot.SlotVision} none="None" label="Vision model" />
      </Row>
      <Row label="Codex" hint="The model Codex uses">
        <ModelPicker role={Slot.SlotCodex} none="Same as main" label="Codex model" />
      </Row>
    {/if}
  {/if}
  {#snippet footer()}
    New sessions use the saved models; running sessions keep theirs.
  {/snippet}
</Section>

<style>
  .pad {
    padding: 9px 12px;
  }
  .disclosure {
    display: flex;
    align-items: center;
    gap: 7px;
    width: 100%;
    height: auto;
    min-height: 36px;
    padding: 7px 12px;
    border-width: 0;
    border-radius: 0;
    background: none;
    box-shadow: none;
    text-align: left;
  }
  .disclosure:hover:not(:disabled) {
    background: none;
  }
  .chev {
    display: inline-block;
    width: 10px;
    color: var(--tertiary);
    font-size: 1.2em;
    line-height: 1;
    transition: transform 0.12s;
  }
  .chev.open {
    transform: rotate(90deg);
  }
  .spacer {
    flex: 1;
  }
  .stack > :global(.row) {
    padding-bottom: 4px;
  }
  .note {
    padding: 0 12px 9px;
    color: var(--secondary);
    font-size: 0.92em;
    text-align: right;
  }
</style>
