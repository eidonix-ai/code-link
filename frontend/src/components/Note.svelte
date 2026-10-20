<script lang="ts">
  import Dot from "./Dot.svelte";
  let { level = "idle", text }: { level?: string; text: string } = $props();
</script>

<div class="note"><Dot {level} /><span>{text}</span></div>

<style>
  .note {
    display: flex;
    align-items: baseline;
    gap: 7px;
    color: var(--secondary);
    font-size: 0.94em;
  }
  .note :global(.dot) {
    transform: translateY(-1px);
  }
</style>
