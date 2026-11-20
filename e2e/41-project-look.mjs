// Project look (WP-65, WP-108): Waypoint picks a new project's emoji, color
// and background itself (no AI); the owner can pick another or reset it to the
// default in the project settings, and what a person chose is never replaced
// except by Pick another. Run after 01-signup-flow (user robin).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const browser = await chromium.launch();

async function run(width, scheme, key) {
  const tag = `${width}/${scheme}`;
  const desktop = width >= 768;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on(
    "console",
    (m) =>
      m.type() === "error" && fail(`${tag} console: ${m.text().slice(0, 300)}`),
  );
  page.on(
    "response",
    (r) =>
      r.status() >= 400 &&
      r.url().includes("/api/") &&
      fail(`${tag} http ${r.status()} ${r.request().method()} ${r.url()}`),
  );
  const api = (m, p, d) =>
    page.request
      .fetch(base + "/api/v1" + p, { method: m, headers: H, data: d })
      .then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({
      path: `${out}/${name}-${width}-${scheme}.png`,
      fullPage: true,
    });
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );
  const project = () => api("GET", `/projects/${key}`);
  // Waits until the project's look is something else than `was`.
  const lookAfter = async (was) => {
    for (let i = 0; i < 50; i++) {
      const p = await project();
      if (p.look !== was.look || p.icon !== was.icon) return p;
      await sleep(200);
    }
    return project();
  };
  const state = page.getByTestId("look-state");

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  // ---- a new project is styled as it is created ----
  const created = await api("POST", "/projects", { name: `Look ${key}`, key });
  const first = created;
  ok(
    first.look === "generated" &&
      first.icon !== "" &&
      /^#[0-9a-f]{6}$/.test(first.color) &&
      first.background !== "",
    `${tag} it was styled (${first.icon} ${first.color} ${first.background})`,
  );

  // A project that comes with its own color is a person's choice.
  const mine = await api("POST", "/projects", {
    name: `Own ${key}`,
    key: key + "X",
    color: "#ef4444",
  });
  const mineLater = await api("GET", `/projects/${key}X`);
  ok(
    mine.look === "custom" &&
      mineLater.look === "custom" &&
      mineLater.icon === "" &&
      mineLater.color === "#ef4444",
    `${tag} a project with its own color is left alone`,
  );

  // ---- the settings: who chose, and the two buttons ----
  await page.goto(`${base}/p/${key}/settings`);
  await state.waitFor();
  ok(
    /Picked automatically/.test(await state.innerText()),
    `${tag} settings say it was picked automatically`,
  );
  if (desktop) {
    await page
      .locator(`aside [data-project=${key}]`)
      .getByText(first.icon)
      .waitFor();
    ok(true, `${tag} the sidebar shows the project's emoji`);
  }
  await overflow("settings");
  await shot("settings-generated");

  // Pick another: a different look, still automatic, no confirmation needed.
  await page.getByRole("button", { name: "Pick another" }).click();
  let again = await lookAfter(first);
  ok(
    again.look === "generated" && again.icon !== first.icon,
    `${tag} Pick another picked a new look (${again.icon})`,
  );
  await page
    .getByTestId("look-state")
    .getByText(/Picked automatically/)
    .waitFor();
  ok(
    (await page.getByRole("dialog").count()) === 0,
    `${tag} Pick another over an automatic look asks nothing`,
  );

  // A person's change makes it theirs; Pick another then asks first.
  // The generated color can itself be #ef4444, which would change nothing.
  const swatch = (await project()).color === "#ef4444" ? "#f97316" : "#ef4444";
  await page.getByRole("button", { name: swatch }).first().click();
  ok(
    await page.getByText("Save or discard your changes below first.").isVisible(),
    `${tag} look buttons wait for unsaved edits`,
  );
  await page.getByRole("button", { name: "Save changes" }).click();
  await state.getByText(/Chosen by you/).waitFor();
  const custom = await project();
  ok(
    custom.look === "custom" && custom.color === swatch,
    `${tag} saving a color makes the look custom`,
  );
  await shot("settings-custom");
  await page.getByRole("button", { name: "Pick another" }).click();
  const dlg = page.getByRole("dialog");
  await dlg.getByText("Replace your look?").waitFor();
  await shot("confirm");
  await dlg.getByRole("button", { name: "Cancel" }).click();
  await sleep(300);
  ok(
    (await project()).look === "custom",
    `${tag} cancelling keeps the person's look`,
  );
  await page.getByRole("button", { name: "Pick another" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Pick another" }).click();
  again = await lookAfter(custom);
  ok(
    again.look === "generated" && again.color !== "#ef4444",
    `${tag} confirming replaces it (${again.icon} ${again.color})`,
  );
  await state.getByText(/Picked automatically/).waitFor();

  // ---- reset to default ----
  await page.getByRole("button", { name: "Reset to default" }).click();
  await state.getByText(/default look/i).waitFor();
  const plain = await project();
  ok(
    plain.look === "plain" && plain.icon === "" && plain.background === "",
    `${tag} Reset to default clears icon and background`,
  );
  ok(
    (await project()).look === "plain",
    `${tag} a reset look stays reset`,
  );
  await overflow("settings after reset");
  await shot("settings-reset");

  // ---- the new project dialog leaves color and icon to Waypoint ----
  if (desktop) {
    await page.goto(base + "/");
    await page.getByRole("button", { name: "New project" }).first().click();
    await page.getByLabel("Name").fill(`Dialog ${key}`);
    await page.getByRole("button", { name: "Create project" }).click();
    await page.waitForURL(/\/p\/[A-Z0-9]+\/board/);
    const dkey = page.url().split("/p/")[1].split("/")[0];
    const dp = await api("GET", `/projects/${dkey}`);
    ok(
      dp.look === "generated" && dp.icon !== "",
      `${tag} a project made in the dialog is styled too (${dp.icon})`,
    );
    await shot("board-styled");
  } else {
    await page.goto(`${base}/p/${key}/board`);
    await overflow("board");
    await shot("board");
  }
  await ctx.close();
}

for (const [w, s, k] of looks([
  [1440, "light", "LKA"],
  [1440, "dark", "LKB"],
  [390, "light", "LKC"],
  [390, "dark", "LKD"],
]))
  try {
    await run(w, s, k);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n").slice(0, 3).join(" ")}`);
  }

await browser.close();
console.log(errors.length ? `${errors.length} problem(s)` : "all good");
process.exit(errors.length ? 1 : 0);
