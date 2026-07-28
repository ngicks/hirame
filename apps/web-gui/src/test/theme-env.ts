import type {
  ColorSchemeMedia,
  PreferenceStorage,
  ThemeEnvironment,
} from "../theme/theme";
import { STORAGE_KEY, createThemeController } from "../theme/theme";

export class FakeMedia implements ColorSchemeMedia {
  matches: boolean;
  private readonly listeners = new Set<(e: { matches: boolean }) => void>();

  constructor(matches: boolean) {
    this.matches = matches;
  }

  addEventListener(_: "change", listener: (e: { matches: boolean }) => void) {
    this.listeners.add(listener);
  }

  removeEventListener(_: "change", listener: (e: { matches: boolean }) => void) {
    this.listeners.delete(listener);
  }

  set(matches: boolean) {
    this.matches = matches;
    for (const listener of this.listeners) {
      listener({ matches });
    }
  }

  get listenerCount() {
    return this.listeners.size;
  }
}

export function fakeStorage(
  initial?: string,
): PreferenceStorage & Map<string, string> {
  const map = new Map<string, string>();
  if (initial !== undefined) {
    map.set(STORAGE_KEY, initial);
  }
  return Object.assign(map, {
    getItem: (key: string) => map.get(key) ?? null,
    setItem: (key: string, value: string) => void map.set(key, value),
    removeItem: (key: string) => void map.delete(key),
  });
}

export function setupTheme(options: { systemDark: boolean; stored?: string }) {
  const media = new FakeMedia(options.systemDark);
  const storage = fakeStorage(options.stored);
  const applied: string[] = [];
  const env: ThemeEnvironment = {
    media,
    storage,
    applyTheme: (theme) => void applied.push(theme),
  };
  return { media, storage, applied, controller: createThemeController(env) };
}
