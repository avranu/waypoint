// Preloaded by run.sh (node --import) into every suite.
//
// Screenshots are evidence for people, not assertions: under CI load
// page.screenshot / locator.screenshot can time out (30s) and, uncaught, that
// killed the whole suite. A screenshot that times out is now logged and
// skipped; any other error (a real failure) still throws.
//
// Shared CI runners are slow when busy: a 10s default timeout turned a
// /login page.goto into a failed suite while the host's load was 7. Default
// timeouts a suite sets are scaled by E2E_SLOW (3 under CI, else 1);
// explicit per-call timeouts are left alone, some of them assert absence.
import { chromium } from "playwright";

const slow = Number(process.env.E2E_SLOW ?? (process.env.CI ? 3 : 1)) || 1;

const wrap = (proto) => {
  const orig = proto.screenshot;
  proto.screenshot = async function (opts) {
    try {
      return await orig.call(this, opts);
    } catch (e) {
      if (e?.name !== "TimeoutError") throw e;
      const name =
        String(opts?.path ?? "")
          .split("/")
          .pop() || "(unnamed)";
      console.log(`shot skipped (timeout): ${name}`);
    }
  };
};

const scale = (proto) => {
  for (const m of ["setDefaultTimeout", "setDefaultNavigationTimeout"]) {
    const orig = proto[m];
    proto[m] = function (ms) {
      return orig.call(this, typeof ms === "number" ? ms * slow : ms);
    };
  }
};

// Page, Locator and BrowserContext classes are only reachable from an instance, so make one.
const launch = chromium.launch.bind(chromium);
let patched = false;
// A task opens on its Summary tab (WP-780); the suites written before it
// check what the Details tab holds (links, files, runs, the work log), so
// every browser starts on Details unless a suite picks Summary itself.
const detailsFirst = (browser) => {
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
};

chromium.launch = async (...args) => {
  const browser = await launch(...args);
  detailsFirst(browser);
  if (!patched) {
    patched = true;
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    wrap(Object.getPrototypeOf(page));
    wrap(Object.getPrototypeOf(page.locator("body")));
    if (slow !== 1) {
      scale(Object.getPrototypeOf(page));
      scale(Object.getPrototypeOf(ctx));
    }
    await ctx.close();
  }
  return browser;
};
