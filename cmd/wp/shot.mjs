// wp shot's browser (WP-245): reads a job on stdin, takes each page at each
// size with Playwright and prints the results as one JSON line. argv[2] is
// a path Playwright resolves from; WP_SHOT_TOKENS (JSON, origin to token)
// signs in to the pages of the Waypoint server and of this checkout's
// preview (wp preview, WP-1226), each token only on its own origin.
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const require = createRequire(process.argv[2]);
let pw;
try {
  pw = require("playwright");
} catch {
  pw = require("@playwright/test");
}
const job = JSON.parse(readFileSync(0, "utf8"));
// Keyed by the origin as this browser writes it (IP and IDN hosts included).
const tokens = Object.fromEntries(
  Object.entries(JSON.parse(process.env.WP_SHOT_TOKENS || "{}")).map(([o, t]) => [new URL(o).origin, t]),
);
const maxHeight = 3;

function slug(u) {
  const x = new URL(u);
  const s = (x.hostname + x.pathname)
    .replace(/[^a-zA-Z0-9]+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 60);
  return s || "page";
}

const browser = await pw.chromium.launch();
const out = [];
try {
  for (const size of job.sizes) {
    // A token goes only to a context that shows its own server's pages, so
    // no other page can make a request that carries it.
    const blocked = [];
    const open = async (server) => {
      const token = (server && tokens[server]) || "";
      const ctx = await browser.newContext({
        viewport: { width: size.width, height: size.height },
        isMobile: size.mobile,
        hasTouch: size.mobile,
        deviceScaleFactor: 1,
      });
      if (token) {
        await ctx.route("**/*", (route) => {
          const req = route.request();
          const origin = new URL(req.url()).origin;
          // Service-worker fetches have no frame; they belong to this server.
          if (req.serviceWorker()) {
            if (origin !== server) return route.continue();
            return route.continue({ headers: { ...req.headers(), authorization: "Bearer " + token } });
          }
          // A page that navigates away (Go.tsx follows a link) must not take
          // the token with it: refuse top-level moves off this server, and
          // send the token only while the page itself is on it.
          if (origin !== server) {
            if (req.isNavigationRequest() && !req.frame().parentFrame()) {
              blocked.push(req.url());
              return route.abort();
            }
            return route.continue();
          }
          const top = req.frame().page()?.mainFrame().url() || "";
          if (top && top !== "about:blank" && new URL(top).origin !== server) return route.continue();
          const furl = req.frame().url();
          const opaque = !furl || furl === "about:blank" || furl.startsWith("data:");
          if (!opaque && new URL(furl).origin !== server) return route.continue();
          if (opaque && req.frame().parentFrame()) return route.continue();
          return route.continue({
            headers: { ...req.headers(), authorization: "Bearer " + token },
          });
        });
      }
      return ctx;
    };
    const ctxs = {};
    let n = 0;
    for (const t of job.targets) {
      n++;
      const r = { url: t.url, size: size.name, file: "", status: 0, title: "", error: "" };
      const origin = t.auth ? new URL(t.url).origin : "";
      if (origin && !tokens[origin]) {
        out.push({ ...r, error: "no token for " + origin + ", so the page would be taken signed out" });
        continue;
      }
      const key = origin || "plain";
      ctxs[key] ??= await open(origin);
      const page = await ctxs[key].newPage();
      blocked.length = 0;
      try {
        const resp = await page.goto(t.url, { waitUntil: "load", timeout: job.timeout_ms });
        r.status = resp ? resp.status() : 0;
        await page.waitForLoadState("networkidle", { timeout: 5000 }).catch(() => {});
        if (job.wait_for) await page.waitForSelector(job.wait_for, { timeout: job.timeout_ms });
        if (job.delay_ms) await page.waitForTimeout(job.delay_ms);
        if (blocked.length) throw new Error("the page navigated to " + blocked[0] + ", which wp shot won't send the token to; shoot that URL on its own");
        r.title = await page.title();
        const height = await page.evaluate(() => document.documentElement.scrollHeight);
        r.file = join(job.dir, `${String(n).padStart(2, "0")}-${slug(t.url)}-${size.name}.png`);
        await page.screenshot({
          path: r.file,
          fullPage: true,
          clip: { x: 0, y: 0, width: size.width, height: Math.min(Math.max(height, size.height), size.height * maxHeight) },
        });
      } catch (e) {
        r.error = String((e && e.message) || e).split("\n")[0];
      }
      await page.close();
      out.push(r);
    }
    for (const c of Object.values(ctxs)) await c.close();
  }
} finally {
  await browser.close();
}
console.log(JSON.stringify(out));
