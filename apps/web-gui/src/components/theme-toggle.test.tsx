import { act, fireEvent, render, screen } from "@testing-library/preact";
import { describe, expect, it } from "vitest";

import { setupTheme } from "../test/theme-env";
import { STORAGE_KEY, THEMES } from "../theme/theme";
import { ThemeToggle } from "./theme-toggle";

const LIGHT = "ライトモード";
const DARK = "ダークモード";
const systemLabel = (mode: string) => `システム設定に従う（現在: ${mode}）`;

function pressed(label: string): boolean {
  const button = screen.getByRole("button", { name: label });
  return button.getAttribute("aria-pressed") === "true";
}

describe("ThemeToggle", () => {
  it("selects light and applies the light theme", () => {
    const { controller, storage, applied } = setupTheme({ systemDark: true });
    render(<ThemeToggle controller={controller} />);

    fireEvent.click(screen.getByRole("button", { name: LIGHT }));

    expect(pressed(LIGHT)).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.light);
    expect(storage.get(STORAGE_KEY)).toBe("light");
  });

  it("selects dark and applies the dark theme", () => {
    const { controller, storage, applied } = setupTheme({ systemDark: false });
    render(<ThemeToggle controller={controller} />);

    fireEvent.click(screen.getByRole("button", { name: DARK }));

    expect(pressed(DARK)).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.dark);
    expect(storage.get(STORAGE_KEY)).toBe("dark");
  });

  it("defaults to the system scheme and keeps following it", () => {
    const { controller, media, applied } = setupTheme({ systemDark: true });
    render(<ThemeToggle controller={controller} />);

    expect(pressed(systemLabel("ダーク"))).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.dark);

    act(() => media.set(false));

    expect(pressed(systemLabel("ライト"))).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.light);
  });

  it("restores a persisted override and ignores later system changes", () => {
    const { controller, media, applied } = setupTheme({
      systemDark: true,
      stored: "light",
    });
    render(<ThemeToggle controller={controller} />);

    expect(pressed(LIGHT)).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.light);

    act(() => {
      media.set(false);
      media.set(true);
    });

    expect(pressed(LIGHT)).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.light);
  });

  it("clears the override when the user returns to the system setting", () => {
    const { controller, storage, applied } = setupTheme({
      systemDark: true,
      stored: "light",
    });
    render(<ThemeToggle controller={controller} />);

    fireEvent.click(screen.getByRole("button", { name: systemLabel("ライト") }));

    expect(storage.has(STORAGE_KEY)).toBe(false);
    expect(pressed(systemLabel("ダーク"))).toBe(true);
    expect(applied.at(-1)).toBe(THEMES.dark);
  });
});
