// Experiments (WP-254): Admin → Experiments lists each A/B test with its arms
// and live run; an admin starts one, reads a run's results and stops it.
// The prep experiment is retired (its run ended by mig 0157: the prep policy replaced it); its results stay.
// Desktop and phone widths, light and dark.
// Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();
const login = async (page) => {
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
};

const runs = [[1440, "light"], [390, "light"], [390, "dark"]];
for (const [n, [width, scheme]] of runs.entries()) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: scheme });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && !/Failed to load resource/.test(m.text()) && fail(`${tag} console: ${m.text().slice(0, 300)}`));
  await login(page);
  const api = (m, p, d) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d }).then((r) => r.json());
  const overflow = async (where) =>
    ok(!(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)), `${tag} no sideways scroll (${where})`);
  const shot = (name) => page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png`, fullPage: true });

  await page.goto(`${base}/admin?tab=experiments`);
  await page.getByTestId("experiments").waitFor();
  const card = (key) => page.locator("section", { has: page.locator(`[data-experiment="${key}"]`) });
  await page.locator("[data-experiment]").first().waitFor();
  ok((await page.locator("[data-experiment]").count()) === 5, `${tag} five experiments show`);
  ok((await page.locator('[data-experiment="prep"]').getAttribute("data-live")) === "no", `${tag} the prep holdout is retired (the prep policy ended its run)`);
  ok(await card("prep").getByText("Prep shown (control) · 4").isVisible(), `${tag} prep's 4:1 split shows`);
  await overflow("list");
  await shot("experiments");

  if (n === 0) {
    const brief = card("next_brief");
    ok((await page.locator('[data-experiment="next_brief"]').getAttribute("data-live")) === "no", `${tag} next_brief is off`);
    await brief.getByRole("button", { name: "Start", exact: true }).click();
    const dlg = page.getByRole("dialog");
    await dlg.getByLabel("Days").fill("5");
    await dlg.getByLabel("Note").fill("does the brief pay?");
    await dlg.getByRole("button", { name: "Start", exact: true }).click();
    await page.locator('[data-experiment="next_brief"][data-live="yes"]').waitFor();
    const l = await api("GET", "/experiments");
    const live = l.experiments.find((x) => x.key === "next_brief").live;
    ok(live && live.note === "does the brief pay?" && live.ends_at, `the run is kept: ${JSON.stringify(live)}`);
    ok(await brief.getByText("does the brief pay?").isVisible(), `${tag} the run's note shows`);

    await brief.getByRole("button", { name: /Run \d+ \(live\)/ }).click();
    const res = brief.getByTestId("experiment-results");
    await res.getByText(/Too few/).first().waitFor();
    ok(await res.locator('tr[data-primary="yes"]').getByText("Cost").isVisible(), `${tag} the deciding metric is marked`);
    await res.getByText(/Caveats \(\d+\)/).click();
    ok(await res.getByText(/heavy-tailed/).first().isVisible(), `${tag} caveats open`);
    await res.getByRole("radio", { name: "All measured" }).click();
    await res.getByText(/Too few/).first().waitFor();
    await overflow("results");
    await shot("experiment-results");

    await brief.getByRole("button", { name: "Stop", exact: true }).click();
    await page.getByRole("dialog").getByLabel("Why").fill("e2e over");
    await page.getByRole("dialog").getByRole("button", { name: "Stop", exact: true }).click();
    await page.locator('[data-experiment="next_brief"][data-live="no"]').waitFor();
    ok(await brief.getByRole("button", { name: /Run \d+ · / }).isVisible(), `${tag} the stopped run's results stay`);
  } else {
    await card("prep").getByRole("button", { name: /Run \d+ · / }).click();
    await card("prep").getByTestId("experiment-results").getByText(/Too few/).first().waitFor();
    await overflow("prep results");
    await shot("prep-results");
  }
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
