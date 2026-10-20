<script lang="ts">
  import Documents from "./Documents.svelte";

  let open = $state(false);
</script>

<div class="more">
  <button class="link" onclick={() => (open = !open)} aria-expanded={open}>
    <span class="chev" class:open>›</span> More
  </button>
</div>
{#if open}
  <Documents />
{/if}

<style>
  .more {
    margin: -4px 2px 16px;
  }
  .chev {
    display: inline-block;
    width: 10px;
    transition: transform 0.12s;
  }
  .chev.open {
    transform: rotate(90deg);
  }
</style>
