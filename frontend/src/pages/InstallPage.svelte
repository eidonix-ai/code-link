<script lang="ts">
  import { onMount, tick } from "svelte";
  import { Settings, onJob, type JobView, type Tool } from "../lib/api";
  import Note from "../components/Note.svelte";

  let { tool }: { tool: Tool } = $props();
  let job = $state<JobView | null>(null);
  let details = $state(false);
  let box: HTMLDivElement | undefined = $state();

  async function show(j: JobView) {
    job = j;
    if (!j.done) details = false;
    await tick();
    if (box) box.scrollTop = box.scrollHeight;
  }

  onMount(() => {
    Settings.Job(tool).then(show);
    return onJob((j) => j.tool === tool && show(j));
  });

  const heading = $derived(!job ? "" : job.done ? (job.ok ? job.message : `Couldn't ${job.verb} ${job.name}`) :
    `${job.verb === "update" ? "Updating" : "Installing"} ${job.name}…`);

  function launch() {
    Settings.OpenLaunch(tool);
    Settings.CloseWindow("install", tool);
  }
</script>

{#if job}
  <div class="page">
    <h1>{heading}</h1>
    {#if job.done && !job.ok}
      <Note level="error" text={job.message} />
      {#if (job.tail ?? []).length > 0}
        <button class="link" onclick={() => (details = !details)}>{details ? "Hide details" : "Show details"}</button>
        {#if details}
          <div class="log mono">{(job.tail ?? []).join("\n")}</div>
        {/if}
      {/if}
    {:else}
      <div class="log mono" bind:this={box}>{(job.lines ?? []).slice(-200).join("\n") || " "}</div>
    {/if}
    <div class="actions">
      {#if job.running}
        <button onclick={() => Settings.CancelJob(tool)}>Cancel</button>
      {:else if job.ok}
        <button onclick={() => Settings.CloseWindow("install", tool)}>Close</button>
        <button class="primary" onclick={launch}>Launch {job.name}</button>
      {:else}
        <button onclick={() => Settings.CloseWindow("install", tool)}>Close</button>
      {/if}
    </div>
  </div>
{/if}

<style>
  .page {
    padding: 20px 22px;
    display: flex;
    flex-direction: column;
    gap: 12px;
    height: 100%;
  }
  h1 {
    margin: 0;
    font-size: 1.2em;
    font-weight: 600;
  }
  .log {
    flex: 1;
    min-height: 120px;
    overflow-y: auto;
    white-space: pre-wrap;
    word-break: break-word;
    padding: 9px 11px;
    background: var(--group);
    border: 1px solid var(--group-border);
    border-radius: var(--radius);
    color: var(--secondary);
    -webkit-user-select: text;
    user-select: text;
  }
  .link {
    align-self: flex-start;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
