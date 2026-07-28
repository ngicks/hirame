import { render } from "preact";

import { App } from "./app";
import "./styles/app.css";
// Imported for its side effect: the controller applies the resolved theme at
// module evaluation, before the first render.
import "./theme/controller";

const root = document.getElementById("app");
if (!root) {
  throw new Error("#app is missing from index.html");
}

render(<App />, root);
