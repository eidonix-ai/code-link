<script lang="ts">
  // code-link's own update: download → check → restart, with Cancel while
  // it is still downloading and the problem in plain words if it fails.
  import { onMount } from "svelte";
  import { Settings, onUpdate, Phase, type UpdateState } from "../lib/api";
  import Note from "../components/Note.svelte";
  import Dot from "../components/Dot.svelte";

  let s = $state<UpdateState | null>(null);
  let details = $state(false);

  onMount(() => {
    Settings.Update().then((u) => (s = u));
    return onUpdate((u) => (s = u));
  });

  const job = $derived(s?.job);
  const failed = $derived(job?.phase === Phase.PhaseFailed || job?.phase === Phase.PhaseCancelled);
  const steps = [
    { label: "Download", phases: [Phase.PhaseDownload] },
    { label: "Check the download", phases: [Phase.PhaseVerify] },
    { label: "Restart code-link", phases: [Phase.PhaseInstall, Phase.PhaseRestart] },
  ];
  function stepState(i: number): "done" | "now" | "todo" {
    const at = steps.findIndex((st) => st.phases.includes(job?.phase as Phase));
    if (at < 0) return "todo";
    return i < at ? "done" : i === at ? "now" : "todo";
  }

  const mb = (n: number) => (n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 0);
  const percent = $derived(job && job.total > 0 ? Math.min(100, Math.round((job.received / job.total) * 100)) : 0);

  const heading = $derived.by(() => {
    if (!job || !job.phase) return "Update code-link";
    if (job.phase === Phase.PhaseCancelled) return "Update cancelled";
    if (job.phase === Phase.PhaseFailed) return s?.restartNeeded ? `code-link ${job.version} is installed` : "Couldn't update code-link";
    if (job.phase === Phase.PhaseRestart && !job.running) return "Restarting code-link…";
    return `Updating code-link to ${job.version}…`;
  });

  function again() {
    details = false;
    Settings.StartUpdate();
  }
</script>

{#if s && job}
  <div class="page">
    <h1>{heading}</h1>
    {#if s.notes && !failed}<p class="secondary notes">{s.notes}</p>{/if}

    {#if failed}
      <Note level={job.phase === Phase.PhaseCancelled ? "idle" : s.restartNeeded ? "warn" : "error"} text={job.message} />
      {#if job.detail}
        <button class="link" onclick={() => (details = !details)}>{details ? "Hide details" : "Show details"}</button>
        {#if details}<div class="detail mono">{job.detail}</div>{/if}
      {/if}
    {:else if job.phase}
      <ol class="steps">
        {#each steps as st, i}
          {@const state = stepState(i)}
          <li class={state}>
            <Dot level={state === "done" ? "ok" : state === "now" ? "warn" : "idle"} />
            <span>{st.label}</span>
            {#if i === 0 && state === "now" && job.total > 0}
              <span class="secondary amount">{mb(job.received)} of {mb(job.total)} MB</span>
            {/if}
          </li>
        {/each}
      </ol>
      {#if job.phase === Phase.PhaseDownload}
        <div class="bar" role="progressbar" aria-valuenow={percent} aria-valuemin={0} aria-valuemax={100}>
          <div style={`width: ${percent}%`}></div>
        </div>
      {/if}
    {/if}

    <div class="spacer"></div>
    <div class="actions">
      {#if job.running}
        <button onclick={() => Settings.CancelUpdate()} disabled={!job.cancellable}>Cancel</button>
      {:else if failed && s.restartNeeded}
        <button onclick={() => Settings.CloseUpdate()}>Close</button>
        <button class="primary" onclick={() => Settings.StartUpdate()}>Quit code-link</button>
      {:else if failed}
        <button onclick={() => Settings.CloseUpdate()}>Close</button>
        {#if s.available}<button class="primary" onclick={again}>Try again</button>{/if}
      {:else if !job.phase}
        <button onclick={() => Settings.CloseUpdate()}>Close</button>
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
  .notes {
    margin: -4px 0 0;
  }
  .steps {
    list-style: none;
    margin: 0;
    padding: 9px 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    background: var(--group);
    border: 1px solid var(--group-border);
    border-radius: var(--radius);
  }
  .steps li {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .steps li.todo {
    color: var(--secondary);
  }
  .amount {
    margin-left: auto;
  }
  .bar {
    height: 6px;
    border-radius: 3px;
    background: var(--separator);
    overflow: hidden;
  }
  .bar > div {
    height: 100%;
    background: var(--accent);
    transition: width 0.2s;
  }
  .detail {
    padding: 9px 11px;
    background: var(--group);
    border: 1px solid var(--group-border);
    border-radius: var(--radius);
    color: var(--secondary);
    white-space: pre-wrap;
    word-break: break-word;
    -webkit-user-select: text;
    user-select: text;
  }
  .link {
    align-self: flex-start;
  }
  .spacer {
    flex: 1;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
  }
</style>
