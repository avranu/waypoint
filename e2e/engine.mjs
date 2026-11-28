// The browser the rich editor suites (83, 84, 85) run under. Chromium unless
// RICH_TYPING_BROWSER=firefox|webkit. RICH_TYPING_WS=ws://host:port/ connects
// to a `playwright run-server` instead of launching one, for an engine whose
// system libraries are not on this host (webkit.sh runs WebKit that way).
import { chromium, firefox, webkit } from "playwright";

export const launchEngine = async () => {
  const name = process.env.RICH_TYPING_BROWSER || "chromium";
  // A name misspelt must not quietly run Chromium in its place.
  const engine = { chromium, firefox, webkit }[name];
  if (!engine) throw new Error(`RICH_TYPING_BROWSER=${name}: chromium, firefox or webkit`);
  const browser = await (process.env.RICH_TYPING_WS
    ? engine.connect(process.env.RICH_TYPING_WS)
    : engine.launch());
  // Tasks open on Summary by default (WP-780); start rich-editor suites on
  // Details across all engines (including firefox/webkit and remote WS, where
  // guard.mjs's chromium.launch patch does not run).
  const newContext = browser.newContext.bind(browser);
  browser.newContext = async (...args) => {
    const ctx = await newContext(...args);
    await ctx.addInitScript(() => {
      try {
        if (localStorage.getItem("wp:issue-tab") === null)
          localStorage.setItem("wp:issue-tab", '"details"');
      } catch {}
    });
    return ctx;
  };
  return browser;
};
