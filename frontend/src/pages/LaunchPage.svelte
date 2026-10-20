<script lang="ts">
  import { onMount } from "svelte";
  import { Settings, onLaunchOpen, onJob, onTools, MenuAction, type LaunchOptions, type LaunchView, type Tool } from "../lib/api";
  import ModelSelect from "../components/ModelSelect.svelte";
  import Note from "../components/Note.svelte";

  let { tool }: { tool: Tool } = $props();
  let view = $state<LaunchView | null>(null);
  let opts = $state<LaunchOptions>({ model: "", heavy: "", background: "", folder: "" });
  let more = $state(false);
  let error = $state("");
  let busy = $state(false);
  let touched = false;

  async function load(reset: boolean) {
    const v = await Settings.LaunchView(tool);
    view = v;
    if (reset || !touched) {
      opts = { ...v.options };
      touched = false;
      error = "";
    }
    // A model that went away: back to the remembered one.
    const ids = new Set((v.groups ?? []).flatMap((g) => (g.items ?? []).map((i) => i.modelID)));
    if (opts.model && !ids.has(opts.model)) opts.model = v.options.model;
  }

  onMount(() => {
    load(true);
    const off = [
      onLaunchOpen((t) => t === tool && load(false)),
      onTools(() => load(false)),
      onJob((j) => j.tool === tool && j.done && load(false)),
    ];
    return () => off.forEach((f) => f());
  });

  function set<K extends keyof LaunchOptions>(k: K, v: LaunchOptions[K]) {
    opts[k] = v;
    touched = true;
    error = "";
  }

  async function change() {
    const f = await Settings.ChooseFolder(tool, opts.folder);
    if (f) set("folder", f);
  }

  async function go() {
    if (busy || !view?.ready || !view.installed || !opts.folder) return;
    busy = true;
    try {
      const r = await Settings.Launch(tool, $state.snapshot(opts) as LaunchOptions);
      error = r.error;
      if (r.missing) await load(false);
      if (!r.error) touched = false;
    } finally {
      busy = false;
    }
  }

  function home(p: string): string {
    return p.replace(/^\/Users\/[^/]+/, "~").replace(/^[A-Za-z]:\\Users\\[^\\]+/, "~");
  }

  const folders = $derived.by(() => {
    const r = view?.recent ?? [];
    return opts.folder && !r.includes(opts.folder) ? [opts.folder, ...r] : r;
  });
</script>

<svelte:window onkeydown={(e) => {
  if (e.key === "Enter" && !(e.target instanceof HTMLButtonElement)) go();
  if (e.key === "Escape") Settings.CloseWindow("launch", tool);
}} />

{#if view}
  <div class="page">
    <h1>Launch {view.toolName}</h1>

    {#if !view.installed}
      <div class="notice">
        <Note level="warn" text={`${view.toolName} isn't installed yet.`} />
        <div class="row-buttons">
          {#if view.toolInfo.busy}
            <span class="secondary">Installing…</span>
          {:else}
            <button onclick={() => Settings.Install(tool, MenuAction.ActInstall)}>Install {view.toolName}</button>
          {/if}
          <button class="link" onclick={() => Settings.OpenURL(view!.toolInfo.installURL)}>Install it yourself</button>
        </div>
      </div>
    {/if}

    {#if !view.ready}
      <div class="notice">
        <Note level="warn" text="There are no models yet." />
        <div class="row-buttons"><button onclick={() => Settings.OpenSettings()}>Open Settings</button></div>
      </div>
    {:else}
      <label class="field">
        <span>Model</span>
        <ModelSelect groups={view.groups ?? []} value={opts.model} label="Model" onchange={(id) => set("model", id)} />
      </label>

      {#if view.tiers}
        <button class="link disclosure" onclick={() => (more = !more)} aria-expanded={more}>
          <span class="chev" class:open={more}>›</span> More options
        </button>
        {#if more}
          <label class="field">
            <span>Heavy model (Opus / Fable)</span>
            <ModelSelect groups={view.groups ?? []} value={opts.heavy} none="Same as the model" label="Heavy model"
              onchange={(id) => set("heavy", id)} />
          </label>
          <label class="field">
            <span>Background model (Haiku)</span>
            <ModelSelect groups={view.groups ?? []} value={opts.background} none="Same as the model" label="Background model"
              onchange={(id) => set("background", id)} />
          </label>
        {/if}
      {/if}

      <div class="field">
        <span>Folder</span>
        <div class="folder">
          {#if folders.length > 0}
            <select aria-label="Folder" value={opts.folder} onchange={(e) => set("folder", e.currentTarget.value)}>
              {#each folders as f (f)}
                <option value={f}>{home(f)}</option>
              {/each}
            </select>
          {:else}
            <span class="secondary">Choose the folder to work in</span>
          {/if}
          <button onclick={change}>Change…</button>
        </div>
      </div>
    {/if}

    {#if error}
      <Note level="error" text={error} />
    {/if}

    <div class="actions">
      <button onclick={() => Settings.CloseWindow("launch", tool)}>Cancel</button>
      <button class="primary" onclick={go} disabled={busy || !view.ready || !view.installed || !opts.folder}>
        {busy ? "Launching…" : "Launch"}
      </button>
    </div>
  </div>
{/if}

<style>
  .page {
    padding: 20px 22px;
    display: flex;
    flex-direction: column;
    gap: 14px;
    height: 100%;
    overflow-y: auto;
  }
  h1 {
    margin: 0 0 2px;
    font-size: 1.25em;
    font-weight: 600;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 5px;
  }
  .field > span {
    color: var(--secondary);
  }
  .field :global(select) {
    width: 100%;
  }
  .folder {
    display: flex;
    gap: 8px;
    align-items: center;
  }
  .folder select {
    flex: 1;
  }
  .folder > span {
    flex: 1;
  }
  .disclosure {
    align-self: flex-start;
  }
  .chev {
    display: inline-block;
    width: 10px;
    transition: transform 0.12s;
  }
  .chev.open {
    transform: rotate(90deg);
  }
  .notice {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px 12px;
    background: var(--group);
    border: 1px solid var(--group-border);
    border-radius: var(--radius);
  }
  .row-buttons {
    display: flex;
    gap: 12px;
    align-items: center;
  }
  .actions {
    margin-top: auto;
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding-top: 6px;
  }
</style>
