<script lang="ts">
  let { checked = $bindable(), label }: { checked?: boolean; label: string } = $props();
</script>

<label class="switch">
  <input type="checkbox" role="switch" aria-label={label} bind:checked />
  <span class="track"><span class="thumb"></span></span>
</label>

<style>
  .switch {
    position: relative;
    display: inline-flex;
  }
  input {
    position: absolute;
    opacity: 0;
    inset: 0;
    margin: 0;
    width: 100%;
    height: 100%;
  }
  .track {
    width: 32px;
    height: 19px;
    border-radius: 10px;
    background: var(--field-border);
    transition: background 0.15s;
    padding: 2px;
  }
  .thumb {
    display: block;
    width: 15px;
    height: 15px;
    border-radius: 50%;
    background: #fff;
    box-shadow: 0 1px 2px rgba(0, 0, 0, 0.25);
    transition: transform 0.15s;
  }
  input:checked + .track {
    background: var(--accent);
  }
  input:checked + .track .thumb {
    transform: translateX(13px);
  }
  input:focus-visible + .track {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
</style>
