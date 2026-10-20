<script lang="ts">
  import type { ChoiceGroup, ChoiceItem } from "../lib/api";

  let { groups, value, none, label, onchange }: {
    groups: ChoiceGroup[];
    value: string;
    /** The label of the "" choice; none = no such choice. */
    none?: string;
    label: string;
    onchange: (id: string) => void;
  } = $props();

  function text(it: ChoiceItem): string {
    return it.facts ? `${it.title}  ·  ${it.facts}` : it.title;
  }
</script>

<select aria-label={label} {value} onchange={(e) => onchange(e.currentTarget.value)}>
  {#if none !== undefined}
    <option value="">{none}</option>
  {/if}
  {#each groups as g (g.backend)}
    <optgroup label={g.backend}>
      {#each g.items ?? [] as it (it.tag)}
        <option value={it.modelID}>{text(it)}</option>
      {/each}
    </optgroup>
  {/each}
</select>
