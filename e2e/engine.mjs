// The browser the rich editor suites (83, 84, 85) run under. Chromium unless
// RICH_TYPING_BROWSER=firefox|webkit. RICH_TYPING_WS=ws://host:port/ connects
// to a `playwright run-server` instead of launching one, for an engine whose
// system libraries are not on this host (webkit.sh runs WebKit that way).
import { chromium, firefox, webkit } from "playwright";

export const launchEngine = () => {
  const engine = { firefox, webkit }[process.env.RICH_TYPING_BROWSER ?? ""] ?? chromium;
  return process.env.RICH_TYPING_WS
    ? engine.connect(process.env.RICH_TYPING_WS)
    : engine.launch();
};
