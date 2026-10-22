<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import type { Backend, Model, Probe } from "../lib/api";
  import Dot from "./Dot.svelte";
  import Note from "./Note.svelte";

  let { backend = $bindable(), open = false }: { backend: Backend; open?: boolean } = $props();
  let expanded = $state(false);
  $effect.pre(() => {
    if (open) expanded = true;
  });

  const probe: Probe | undefined = $derived(settings.probes[backend.id]);
  const inUse = $derived((settings.draft.models ?? []).filter((m) => m.backend === backend.id).length);

  function host(u: string): string {
    try {
      return new URL(u).host;
    } catch {
      return u;
    }
  }

  function summary(p: Probe): string {
    const n = p.models?.length ?? 0;
    const parts = [`${n} model${n === 1 ? "" : "s"}`];
    if (p.models?.some((m) => m.logprobs)) parts.push("logprobs");
    if (p.models?.some((m) => m.vision)) parts.push("vision");
    if (p.latencyMS) parts.push(`${p.latencyMS} ms`);
    return "Reachable · " + parts.join(" · ");
  }

  /** What a gateway offers besides chat: "web search · documents". */
  function offers(p: Probe | undefined): string {
    const g = p?.gateway;
    if (!g) return "";
    return [g.webSearch ? "web search" : "", g.documents ? "documents" : ""].filter((x) => x).join(" · ");
  }

  /** Whether Claude Code's auto-mode classifier goes to this model's provider as is. */
  function native(m: Model): boolean {
    return m.nativeClassifier ?? backend.nativeClassifier?.includes(m.upstream) ?? false;
  }

  const providerModels = $derived((settings.draft.models ?? []).filter((m) => m.backend === backend.id));
  const nativeHint =
    "Claude Code's auto-approval checks are sent straight to this provider's Messages API, so it can reuse the session it already has cached instead of re-reading the whole conversation for every check. Only for models whose server supports it (Flare). Turned on automatically when the provider says it supports it.";

  const localDefault = "http://localhost:8000/v1";

  /** Switching to Go-LLM-Proxy fills in its address; switching away undoes that. */
  function setKind(kind: string) {
    const url = (backend.baseURL ?? "").trim();
    if (kind === "gollm" && (url === "" || url === localDefault)) backend.baseURL = settings.platform.gatewayURL;
    if (kind !== "gollm" && url === settings.platform.gatewayURL) backend.baseURL = localDefault;
    backend.kind = kind as Backend["kind"];
    settings.refresh();
  }

  function joinURL(base: string): string {
    try {
      return new URL(base).origin + "/join";
    } catch {
      return "";
    }
  }

  async function rename(e: Event & { currentTarget: HTMLInputElement }) {
    const input = e.currentTarget;
    const old = backend.id;
    await settings.renameProvider(old, input.value);
    input.value = settings.draft.backends?.some((b) => b.id === input.value.trim()) ? input.value.trim() : old;
  }






</script>

<div class="provider">
  <button class="head" onclick={() => (expanded = !expanded)} aria-expanded={expanded}>
    <Dot level={probe ? (probe.reachable ? "ok" : "error") : "idle"} />
    <span class="name">{backend.id}</span>
    <span class="secondary host">{host(backend.baseURL)}</span>
    <span class="spacer"></span>
    {#if offers(probe)}<span class="secondary">{offers(probe)}</span>{/if}
    {#if inUse > 0}<span class="secondary">{inUse} model{inUse === 1 ? "" : "s"}</span>{/if}
    <span class="chev" class:open={expanded}>›</span>
  </button>

  {#if expanded}
    <div class="body">
      <label class="field">
        <span>Name</span>
        <input type="text" value={backend.id} onchange={rename} spellcheck="false" />
      </label>
      <label class="field">
        <span>Kind</span>
        <select value={backend.kind} onchange={(e) => setKind(e.currentTarget.value)}>
          <option value="openai">OpenAI-compatible (vLLM, SGLang, Flare)</option>
          <option value="gollm">Go-LLM-Proxy</option>
          <option value="anthropic">Anthropic</option>
        </select>
      </label>
      <label class="field">
        <span>Base URL</span>
        <input type="url" bind:value={backend.baseURL} placeholder="http://host:8000/v1" spellcheck="false" />
      </label>
      <label class="field">
        <span>API key</span>
        <input type="password" bind:value={backend.apiKey} onchange={() => settings.refresh()}
          placeholder={backend.kind === "gollm" ? "Required" : "Optional"} autocomplete="off" />
      </label>
      {#if backend.kind === "gollm" && !backend.apiKey && joinURL(backend.baseURL)}
        <div class="field">
          <span></span>
          <div><button class="link" onclick={() => settings.openURL(joinURL(backend.baseURL))}>Get a key</button></div>
        </div>
      {/if}
      <div class="field">
        <span>Connection</span>
        <div class="test">
          {#if settings.probing[backend.id]}
            <span class="secondary">Testing…</span>
          {:else if probe}
            <Note level={probe.reachable ? "ok" : "error"}
              text={probe.reachable ? summary(probe) + (offers(probe) ? " · also " + offers(probe) : "") : probe.error || "Unreachable"} />
          {:else}
            <span class="secondary">Not tested</span>
          {/if}
          <button onclick={() => settings.probe(backend.id)} disabled={settings.probing[backend.id]}>Test</button>
        </div>
      </div>

      {#if providerModels.length > 0}
        <div class="field">
          <span class="hinted" title={nativeHint}>Native auto-approval</span>
          <div class="chips">
            {#each providerModels.filter(native) as m (m.id)}
              <span class="chip">{m.id}<button class="x" aria-label={"Turn off for " + m.id} onclick={() => (m.nativeClassifier = false)}>×</button></span>
            {/each}
            {#if providerModels.some((m) => !native(m))}
              <select value="" onchange={(e) => { const m = providerModels.find((x) => x.id === e.currentTarget.value); if (m) m.nativeClassifier = true; e.currentTarget.value = ""; }}>
                <option value="" disabled>Add model…</option>
                {#each providerModels.filter((m) => !native(m)) as m (m.id)}<option value={m.id}>{m.id}</option>{/each}
              </select>
            {/if}
          </div>
        </div>
      {/if}

      <div class="actions">
        <button class="link" onclick={() => settings.removeProvider(backend.id)}>Remove provider</button>
      </div>
    </div>
  {/if}
</div>

<style>
  .head {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    height: auto;
    min-height: 40px;
    padding: 8px 12px;
    border-width: 0;
    border-radius: 0;
    background: none;
    box-shadow: none;
    text-align: left;
  }
  .head:hover:not(:disabled) {
    background: none;
  }
  .name {
    font-weight: 500;
  }
  .host {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .spacer {
    flex: 1;
  }
  .chev {
    color: var(--tertiary);
    font-size: 1.2em;
    line-height: 1;
    transition: transform 0.12s;
  }
  .chev.open {
    transform: rotate(90deg);
  }
  .body {
    padding: 2px 12px 10px 27px;
    display: flex;
    flex-direction: column;
    gap: 7px;
  }
  .field {
    display: grid;
    grid-template-columns: 90px 1fr;
    align-items: center;
    gap: 10px;
  }
  .field > span {
    color: var(--secondary);
  }
  .test {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    min-height: 24px;
  }
  .hinted {
    text-decoration: underline dashed;
    text-underline-offset: 3px;
    cursor: help;
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 1px 4px 1px 8px;
    border: 1px solid var(--border, currentColor);
    border-radius: 10px;
  }
  .chip .x {
    height: auto;
    min-height: 0;
    padding: 0 4px;
    border-width: 0;
    background: none;
    box-shadow: none;
    color: var(--secondary);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    margin-top: 2px;
  }
</style>
