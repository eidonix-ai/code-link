<script lang="ts">
  let { level = "idle" }: { level?: string } = $props();
</script>

<span class="dot {level}" aria-hidden="true"></span>

<style>
  .dot {
    display: inline-block;
    flex: none;
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--idle);
  }
  .ok {
    background: var(--ok);
  }
  .warn,
  .warning {
    background: var(--warn);
  }
  .error {
    background: var(--error);
  }
</style>
