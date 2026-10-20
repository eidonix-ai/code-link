<script lang="ts">
  import { settings } from "../lib/state.svelte";
  import Section from "./Section.svelte";
  import Row from "./Row.svelte";
</script>

<Section title="Share settings">
  <Row label="Settings file" hint="Your providers and models in a .codelink file — never your keys">
    <button onclick={() => settings.exportSettings()} disabled={settings.dirty} title={settings.dirty ? "Save first" : ""}>Export…</button>
    <button onclick={() => settings.chooseImport()}>Import…</button>
  </Row>
</Section>
