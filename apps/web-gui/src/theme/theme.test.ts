import { describe, expect, it } from "vitest";

import { setupTheme as setup } from "../test/theme-env";
import { STORAGE_KEY, THEMES } from "./theme";

describe("createThemeController", () => {
  it("follows the system scheme when nothing is stored", () => {
    const { controller, applied } = setup({ systemDark: true });

    expect(controller.preference.value).toBe("system");
    expect(controller.mode.value).toBe("dark");
    expect(applied.at(-1)).toBe(THEMES.dark);
  });

  it("reacts to later system changes while unset", () => {
    const { controller, media, applied } = setup({ systemDark: false });
    expect(controller.mode.value).toBe("light");

    media.set(true);

    expect(controller.mode.value).toBe("dark");
    expect(applied.at(-1)).toBe(THEMES.dark);
  });

  it("persists an explicit override and stops following the system", () => {
    const { controller, media, storage, applied } = setup({ systemDark: true });

    controller.toggle();

    expect(controller.preference.value).toBe("light");
    expect(storage.get(STORAGE_KEY)).toBe("light");
    expect(applied.at(-1)).toBe(THEMES.light);

    media.set(false);
    media.set(true);

    expect(controller.mode.value).toBe("light");
    expect(applied.at(-1)).toBe(THEMES.light);
  });

  it("restores a stored override over the system scheme", () => {
    const { controller, applied } = setup({ systemDark: true, stored: "light" });

    expect(controller.preference.value).toBe("light");
    expect(applied.at(-1)).toBe(THEMES.light);
  });

  it("ignores an unrecognized stored value", () => {
    const { controller } = setup({ systemDark: false, stored: "sepia" });

    expect(controller.preference.value).toBe("system");
    expect(controller.mode.value).toBe("light");
  });

  it("clears the override when the user returns to system", () => {
    const { controller, media, storage } = setup({
      systemDark: false,
      stored: "dark",
    });

    controller.setPreference("system");

    expect(storage.has(STORAGE_KEY)).toBe(false);
    expect(controller.mode.value).toBe("light");

    media.set(true);
    expect(controller.mode.value).toBe("dark");
  });

  it("keeps working when its methods are detached from the controller", () => {
    const { controller, applied } = setup({ systemDark: false });
    const { toggle, setPreference } = controller;

    toggle();
    expect(applied.at(-1)).toBe(THEMES.dark);

    setPreference("system");
    expect(controller.preference.value).toBe("system");
  });

  it("detaches its system listener on dispose", () => {
    const { controller, media } = setup({ systemDark: false });
    expect(media.listenerCount).toBe(1);

    controller.dispose();

    expect(media.listenerCount).toBe(0);
  });
});
