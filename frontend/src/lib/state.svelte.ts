// The Settings page's state: the saved settings, the draft being edited and
// what the Go side derives from it. Edits with rules (roles, providers,
// model ticks) go through the Settings service; plain fields are bound to
// the draft directly.
import {
  Settings, canonical, clone, onActivity, onSettingsOpen, onStatus,
  type CheckResult, type ChoiceGroup, type Config, type Entry, type Probe, type Snapshot, type Status, type View, Slot,
} from "./api";

type Platform = Snapshot["platform"];
export type Message = { level: "ok" | "warn" | "error"; text: string } | null;

const emptyConfig = (): Config => ({
  port: 0, accessToken: "", backends: [], models: [],
  roles: { main: "" }, documents: {}, webSearch: {}, launch: {},
});

/** Fills the optional fields the pickers bind to, so they show their "off" / "automatic" choice. */
function normalize(c: Config): Config {
  c.backends ??= [];
  c.models ??= [];
  c.roles ??= { main: "" };
  c.documents ??= {};
  c.documents.local ||= "ocr" as never;
  c.documents.paddleOCR ??= "";
  c.webSearch ??= {};
  c.webSearch.provider ??= "" as never;
  c.webSearch.apiKey ??= "";
  c.launch ??= {};
  c.launch.terminal ??= "";
  c.launch.permissionMode ??= "";
  c.launch.openAtLogin ??= false;
  return c;
}

class SettingsState {
  loaded = $state(false);
  saved = $state<Config>(emptyConfig());
  draft = $state<Config>(emptyConfig());
  groups = $state<ChoiceGroup[]>([]);
  decision = $state("");
  warnings = $state<string[]>([]);
  status = $state<Status>({ kind: "notConfigured" as Status["kind"], line: "", port: 0, level: "idle" });
  platform = $state<Platform>({ os: "darwin", terminals: [], permissionModes: [], defaultPort: 18775 });
  probes = $state<Record<string, Probe>>({});
  probing = $state<Record<string, boolean>>({});
  activity = $state<Entry[]>([]);
  loadWarning = $state("");
  message = $state<Message>(null);
  saving = $state(false);
  path = $state("");

  dirty = $derived(this.loaded && canonical(this.draft) !== canonical(this.saved));

  private queue: Promise<unknown> = Promise.resolve();
  private saves = 0;

  /** Runs calls one at a time, in order, so an edit never overtakes a save. */
  private run<T>(f: () => Promise<T>): Promise<T> {
    const p = this.queue.then(f, f);
    this.queue = p.catch(() => undefined);
    return p;
  }

  snapshot(): Config {
    return $state.snapshot(this.draft) as Config;
  }

  private apply(v: View) {
    this.draft = normalize(v.draft);
    this.groups = v.groups ?? [];
    this.decision = v.decision;
    if (v.warning) this.warnings = [...this.warnings, v.warning];
  }

  async load() {
    const s = await this.run(() => Settings.Load());
    this.saved = s.saved;
    this.platform = s.platform;
    this.status = s.status;
    this.probes = (s.probes ?? {}) as Record<string, Probe>;
    this.activity = s.activity ?? [];
    this.loadWarning = s.loadWarning;
    this.path = s.path;
    if (!this.loaded || !this.dirty) {
      this.draft = normalize(clone(s.saved));
      this.groups = s.view.groups ?? [];
      this.decision = s.view.decision;
    }
    this.loaded = true;
    for (const b of this.draft.backends ?? []) {
      if (!this.probes[b.id]) this.probe(b.id);
    }
  }

  listen() {
    const off = [
      onStatus((s) => (this.status = s)),
      onActivity((e) => (this.activity = [e, ...this.activity].slice(0, 200))),
      onSettingsOpen(() => this.load()),
    ];
    return () => off.forEach((f) => f());
  }

  async edit(f: (d: Config) => Promise<View>) {
    this.apply(await this.run(() => f(this.snapshot())));
  }

  /** Re-derives the pickers after the page changed the draft itself. */
  async refresh() {
    await this.edit((d) => Settings.Describe(d));
  }

  setRole(slot: Slot, tag: string) {
    return this.edit((d) => Settings.SetRole(d, slot, tag));
  }
  setEnabled(tag: string, on: boolean) {
    return this.edit((d) => Settings.SetEnabled(d, tag, on));
  }
  renameModel(old: string, name: string) {
    return this.edit((d) => Settings.RenameModel(d, old, name));
  }
  addProvider() {
    return this.edit((d) => Settings.AddProvider(d));
  }
  renameProvider(old: string, name: string) {
    if (this.probes[old]) {
      this.probes[name] = this.probes[old];
      delete this.probes[old];
    }
    return this.edit((d) => Settings.RenameProvider(d, old, name));
  }
  removeProvider(id: string) {
    delete this.probes[id];
    return this.edit((d) => Settings.RemoveProvider(d, id));
  }

  async probe(id: string) {
    const b = this.draft.backends?.find((x) => x.id === id);
    if (!b || this.probing[id]) return;
    this.probing[id] = true;
    try {
      const p = await Settings.Probe($state.snapshot(b));
      this.probes[id] = p;
      await this.refresh();
    } finally {
      this.probing[id] = false;
    }
  }

  testOCR(url: string): Promise<CheckResult> {
    return Settings.TestOCR(url);
  }

  testSearch(): Promise<CheckResult> {
    const w = this.draft.webSearch;
    return Settings.TestSearch(w.provider ?? ("" as never), w.apiKey ?? "");
  }

  async save() {
    if (this.saving) return;
    this.saving = true;
    try {
      const r = await this.run(() => Settings.Save(this.snapshot()));
      this.status = r.status;
      if (!r.ok) {
        this.message = { level: "error", text: (r.problems ?? []).join(" ") };
        return;
      }
      this.saved = r.saved;
      this.draft = normalize(clone(r.saved));
      this.warnings = [];
      this.loadWarning = "";
      const msg: Message = r.note ? { level: "warn", text: r.note } : { level: "ok", text: "Saved" };
      this.message = msg;
      const n = ++this.saves;
      if (msg.level === "ok") setTimeout(() => n === this.saves && (this.message = null), 2500);
      await this.refresh();
    } finally {
      this.saving = false;
    }
  }

  async revert() {
    this.draft = normalize(clone(this.saved));
    this.warnings = [];
    this.message = null;
    await this.refresh();
  }

  async clearActivity() {
    await Settings.ClearActivity();
    this.activity = [];
  }
}

export const settings = new SettingsState();
export { Slot };
