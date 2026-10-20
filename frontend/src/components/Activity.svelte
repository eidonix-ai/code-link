<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Dot from "./Dot.svelte";

  let open = $state(false);
  const time = (t: string) => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
</script>

<section>
  <div class="group">
    <button class="disclosure" onclick={() => (open = !open)} aria-expanded={open}>
      <span class="chev" class:open>›</span>
      <span>Recent activity</span>
      <span class="spacer"></span>
      {#if !open && settings.activity.length > 0}<span class="secondary">{settings.activity.length}</span>{/if}
    </button>
    {#if open}
      <div class="list">
        {#if settings.activity.length === 0}
          <div class="secondary">Nothing yet.</div>
        {:else}
          {#each settings.activity.slice(0, 50) as e (e.id)}
            <div class="line">
              <span class="secondary mono when">{time(e.time)}</span>
              {#if e.level !== "normal"}<Dot level={e.level} />{/if}
              <span class="text" title={e.text}>{e.text}</span>
            </div>
          {/each}
          <div class="clear"><button class="link" onclick={() => settings.clearActivity()}>Clear</button></div>
        {/if}
      </div>
    {/if}
  </div>
</section>

<style>
  section {
    margin: 0 0 22px;
  }
  .group {
    background: var(--group);
    border: 1px solid var(--group-border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .disclosure {
    display: flex;
    align-items: center;
    gap: 7px;
    width: 100%;
    height: auto;
    min-height: 38px;
    padding: 8px 12px;
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
  .list {
    border-top: 1px solid var(--separator);
    padding: 8px 12px 10px;
    display: flex;
    flex-direction: column;
    gap: 3px;
    font-size: 0.92em;
    -webkit-user-select: text;
    user-select: text;
  }
  .line {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }
  .when {
    flex: none;
  }
  .text {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .clear {
    margin-top: 6px;
  }
</style>
