import { cleanup } from "@testing-library/preact";
import { afterEach } from "vitest";

// jsdom implements neither of these, and both are reached during module
// evaluation or the first paint rather than through a seam a test could
// stand in for.
window.matchMedia = ((query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addListener: () => {},
  removeListener: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  dispatchEvent: () => false,
})) as typeof window.matchMedia;

let nextObjectUrl = 0;
URL.createObjectURL = () => `blob:hirame/${nextObjectUrl++}`;
URL.revokeObjectURL = () => {};

afterEach(cleanup);
