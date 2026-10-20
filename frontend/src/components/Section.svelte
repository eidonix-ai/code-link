<script lang="ts">
  import type { Snippet } from "svelte";
  let { title, footer, children }: { title?: string; footer?: Snippet; children: Snippet } = $props();
</script>

<section>
  {#if title}<h2>{title}</h2>{/if}
  <div class="group">{@render children()}</div>
  {#if footer}<div class="foot">{@render footer()}</div>{/if}
</section>

<style>
  section {
    margin: 0 0 22px;
  }
  h2 {
    margin: 0 0 7px 2px;
    font-size: 1em;
    font-weight: 600;
  }
  .group {
    background: var(--group);
    border: 1px solid var(--group-border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .group > :global(* + *) {
    border-top: 1px solid var(--separator) !important;
  }
  .foot {
    margin: 6px 2px 0;
    color: var(--secondary);
    font-size: 0.92em;
  }
</style>
