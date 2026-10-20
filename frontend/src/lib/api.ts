// The Settings service and its events, as the page uses them.
import { Events } from "@wailsio/runtime";
import * as Settings from "../../bindings/codelink/internal/gui/settings.js";
import * as AppEvents from "../../bindings/events/index.js";
import type { Config } from "../../bindings/codelink/internal/config/models.js";
import type { Entry, Status } from "../../bindings/codelink/internal/app/models.js";

export { Settings };
export type { Config, Backend, Model } from "../../bindings/codelink/internal/config/models.js";
export type {
  View, ChoiceGroup, ChoiceItem, Status, Entry, CheckResult, SaveResult, KeyNeed, Option, ImportPreview,
} from "../../bindings/codelink/internal/app/models.js";
export type { Probe, ProbedModel } from "../../bindings/codelink/internal/core/models.js";
export type { Snapshot } from "../../bindings/codelink/internal/gui/models.js";
export { Slot } from "../../bindings/codelink/internal/app/models.js";

export function onStatus(f: (s: Status) => void) {
  return Events.On(AppEvents.Status, (e) => f(e.data as Status));
}

export function onActivity(f: (e: Entry) => void) {
  return Events.On(AppEvents.Activity, (e) => f(e.data as Entry));
}

export function onSettingsOpen(f: () => void) {
  return Events.On(AppEvents.SettingsOpen, () => f());
}

/** A deep copy that keeps the draft independent of what it came from. */
export function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v));
}

/**
 * The draft as a comparable string: keys sorted, and the values the Go side
 * omits (empty strings, nulls, false, empty lists) dropped, so an edit that
 * puts a field back the way it was isn't "unsaved".
 */
export function canonical(c: Config): string {
  const walk = (v: unknown): unknown => {
    if (Array.isArray(v)) return v.map(walk);
    if (v && typeof v === "object") {
      const out: Record<string, unknown> = {};
      for (const k of Object.keys(v as object).sort()) {
        const x = walk((v as Record<string, unknown>)[k]);
        if (x === "" || x === null || x === undefined || x === false) continue;
        if (Array.isArray(x) && x.length === 0) continue;
        out[k] = x;
      }
      return out;
    }
    return v;
  };
  const { accessToken: _t, launch, ...rest } = c;
  const { recentFolders: _r, ...l } = launch ?? {};
  return JSON.stringify(walk({ ...rest, launch: l }));
}

/** "262k" / "1M" for a context window. */
export function shortContext(n: number): string {
  if (n >= 1_000_000) return `${Math.round(n / 1_000_000)}M`;
  if (n >= 1000) return `${Math.floor(n / 1000)}k`;
  return String(n);
}
