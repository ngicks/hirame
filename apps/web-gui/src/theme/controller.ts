import { browserThemeEnvironment, createThemeController } from "./theme";

/**
 * The application-wide controller. Created on import so the stored preference
 * is applied before the first render.
 */
export const themeController = createThemeController(browserThemeEnvironment());
