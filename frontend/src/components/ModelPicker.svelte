<script lang="ts">
  import { settings, Slot } from "../lib/state.svelte";
  import type { ChoiceItem, Config } from "../lib/api";

  let { role, none, label }: { role: Slot; none?: string; label: string } = $props();

  function tagFor(c: Config, s: Slot): string {
    const id = (c.roles as unknown as Record<string, string | undefined>)[s] ?? "";
    const m = c.models?.find((x) => x.id === id);
    return m ? `${m.backend}\u0001${m.upstream}` : "";
  }

  function text(it: ChoiceItem): string {
    let s = it.title;
    if (it.facts) s += "  ·  " + it.facts;
    if (!it.enabled) s += "  (not in use)";
    return s;
  }

  const current = $derived(tagFor(settings.draft, role));
</script>

<select aria-label={label} value={current} onchange={(e) => settings.setRole(role, e.currentTarget.value)}>
  {#if none !== undefined || current === ""}
    <option value="">{none ?? "Choose a model…"}</option>
  {/if}
  {#each settings.groups as g (g.backend)}
    <optgroup label={g.backend}>
      {#each g.items ?? [] as it (it.tag)}
        <option value={it.tag}>{text(it)}</option>
      {/each}
    </optgroup>
  {/each}
</select>
