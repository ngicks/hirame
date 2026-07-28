import { computed, effect, signal } from "@preact/signals";
import type { ReadonlySignal } from "@preact/signals";

/**
 * The two project-owned daisyUI themes. Their definitions live in
 * src/styles/app.css; this map is the only place their names appear in code.
 */
export const THEMES = {
  light: "hirame-light",
  dark: "hirame-dark",
} as const;

export type ColorMode = keyof typeof THEMES;

/** "system" means "no explicit choice": follow prefers-color-scheme. */
export type ThemePreference = ColorMode | "system";

export const STORAGE_KEY = "hirame:color-mode";

export const DARK_MEDIA_QUERY = "(prefers-color-scheme: dark)";

type MediaChangeListener = (event: { matches: boolean }) => void;

/** The MediaQueryList surface this module uses. */
export interface ColorSchemeMedia {
  readonly matches: boolean;
  addEventListener(type: "change", listener: MediaChangeListener): void;
  removeEventListener(type: "change", listener: MediaChangeListener): void;
}

export type PreferenceStorage = Pick<
  Storage,
  "getItem" | "setItem" | "removeItem"
>;

/**
 * Every browser capability the controller touches, injected rather than
 * reached for: jsdom has no matchMedia, and a prerender has no window at all.
 */
export interface ThemeEnvironment {
  storage: PreferenceStorage;
  media: ColorSchemeMedia;
  applyTheme(theme: string): void;
}

export interface ThemeController {
  /** What the user chose, including "system" for no explicit choice. */
  readonly preference: ReadonlySignal<ThemePreference>;
  /** The mode actually in effect once "system" is resolved. */
  readonly mode: ReadonlySignal<ColorMode>;
  /** The daisyUI theme name applied to the document. */
  readonly theme: ReadonlySignal<string>;
  setPreference(preference: ThemePreference): void;
  /** Flips to the opposite of the current mode as an explicit choice. */
  toggle(): void;
  dispose(): void;
}

export function createThemeController(env: ThemeEnvironment): ThemeController {
  const preference = signal<ThemePreference>(readPreference(env.storage));
  const systemMode = signal<ColorMode>(env.media.matches ? "dark" : "light");

  const mode = computed<ColorMode>(() =>
    preference.value === "system" ? systemMode.value : preference.value,
  );
  const theme = computed(() => THEMES[mode.value]);

  const onSystemChange: MediaChangeListener = (event) => {
    systemMode.value = event.matches ? "dark" : "light";
  };
  env.media.addEventListener("change", onSystemChange);

  const stopApplying = effect(() => {
    env.applyTheme(theme.value);
  });

  // Arrow functions, not shorthand methods: components pass these straight to
  // event handlers, where a `this`-dependent method would be unbound.
  const setPreference = (next: ThemePreference) => {
    preference.value = next;
    if (next === "system") {
      env.storage.removeItem(STORAGE_KEY);
    } else {
      env.storage.setItem(STORAGE_KEY, next);
    }
  };

  return {
    preference,
    mode,
    theme,
    setPreference,
    toggle: () => setPreference(mode.value === "dark" ? "light" : "dark"),
    dispose: () => {
      env.media.removeEventListener("change", onSystemChange);
      stopApplying();
    },
  };
}

export function browserThemeEnvironment(
  win: Window = window,
): ThemeEnvironment {
  return {
    storage: win.localStorage,
    media: win.matchMedia(DARK_MEDIA_QUERY),
    applyTheme(theme) {
      win.document.documentElement.dataset["theme"] = theme;
    },
  };
}

function readPreference(storage: PreferenceStorage): ThemePreference {
  const stored = storage.getItem(STORAGE_KEY);
  return stored === "light" || stored === "dark" ? stored : "system";
}
