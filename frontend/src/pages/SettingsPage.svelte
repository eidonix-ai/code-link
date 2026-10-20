<script lang="ts">
  import { onMount } from "svelte";
  import { settings } from "../lib/state.svelte";
  import Note from "../components/Note.svelte";
  import Providers from "../components/Providers.svelte";
  import DefaultModel from "../components/DefaultModel.svelte";
  import More from "../components/More.svelte";
  import SetUp from "../components/SetUp.svelte";
  import WebSearch from "../components/WebSearch.svelte";
  import Launch from "../components/Launch.svelte";
  import Activity from "../components/Activity.svelte";
  import Share from "../components/Share.svelte";
  import GetStarted from "../components/GetStarted.svelte";
  import KeysNeeded from "../components/KeysNeeded.svelte";
  import ImportPreview from "../components/ImportPreview.svelte";
  import Footer from "../components/Footer.svelte";

  onMount(() => {
    const off = settings.listen();
    settings.load();
    return off;
  });

  function keydown(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      if (settings.dirty) settings.save();
    }
  }
</script>

<svelte:window onkeydown={keydown} />

{#if settings.loaded && settings.getStarted}
  <GetStarted need={settings.getStarted} />
{:else if settings.setUp}
  <SetUp />
{:else}
  <div class="page">
    <main>
      {#if settings.loaded}
        {#if settings.loadWarning}
          <div class="banner"><Note level="warn" text={settings.loadWarning} /></div>
        {/if}
        {#if settings.importPreview}
          <ImportPreview />
        {/if}
        {#if settings.needs.length > 0}
          <KeysNeeded />
        {/if}
        <Providers />
        <Launch />
        <DefaultModel />
        <WebSearch />
        <More />
        <Share />
        <Activity />
      {/if}
    </main>
    <Footer />
  </div>
{/if}

<style>
  .page {
    display: flex;
    flex-direction: column;
    height: 100%;
  }
  main {
    flex: 1;
    overflow-y: auto;
    padding: 18px 20px 4px;
  }
  .banner {
    margin: 0 2px 16px;
  }
</style>
