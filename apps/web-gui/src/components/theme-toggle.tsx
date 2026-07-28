import { themeController } from "../theme/controller";
import type { ColorMode, ThemeController, ThemePreference } from "../theme/theme";

const MODE_LABELS: Record<ColorMode, string> = {
  light: "ライト",
  dark: "ダーク",
};

const OPTIONS: ReadonlyArray<{
  preference: ThemePreference;
  icon: string;
  label: string;
}> = [
  { preference: "light", icon: "☀️", label: "ライト" },
  { preference: "dark", icon: "🌙", label: "ダーク" },
  { preference: "system", icon: "🖥️", label: "システム" },
];

/**
 * The controller is a prop so a test can drive one built on a fake environment;
 * the application-wide singleton is the default.
 */
export function ThemeToggle({
  controller = themeController,
}: {
  controller?: ThemeController;
}) {
  const preference = controller.preference.value;
  const mode = controller.mode.value;

  return (
    <div class="join" role="group" aria-label="表示モード">
      {OPTIONS.map((option) => {
        const selected = option.preference === preference;
        return (
          <button
            key={option.preference}
            type="button"
            class={`btn btn-sm join-item ${selected ? "btn-primary" : "btn-ghost"}`}
            aria-pressed={selected}
            aria-label={
              option.preference === "system"
                ? `システム設定に従う（現在: ${MODE_LABELS[mode]}）`
                : `${option.label}モード`
            }
            onClick={() => controller.setPreference(option.preference)}
          >
            <span aria-hidden="true">{option.icon}</span>
            <span class="hidden sm:inline" aria-hidden="true">
              {option.label}
            </span>
          </button>
        );
      })}
    </div>
  );
}
