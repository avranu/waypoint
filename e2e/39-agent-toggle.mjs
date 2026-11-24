// Checks: one control decides who can take a task (WP-93, reshaped by WP-1196). Agents
// are picked in the Assignee list ("Any agent", families, models); a "Humans only"
// toggle locks it to people. The stored `agent` value: '' (Anyone, the default),
// 'none' (Humans only), 'any' or a spec. The lock hides where it would say nothing:
// locked with a person on it, or unlocked with an agent on it. Same on the issue page,
// in the new-issue dialog and the list's bulk bar. At 1440 and 390 wide.
// Needs user robin (run 01-signup-flow first).
import { chromium } from "playwright";
import fs from "node:fs";
import { looks } from "./looks.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

const browser = await chromium.launch();
for (const vp of looks([
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
])) {
  const tag = `${vp.width}px`;
  const ctx = await browser.newContext({ viewport: vp });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fail(`pageerror (${tag}): ` + e.message));
  page.on(
    "console",
    (m) =>
      m.type() === "error" &&
      fail(`console (${tag}): ` + m.text().slice(0, 300)),
  );
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  const api = {
    get: async (p) =>
      (await page.request.get(base + "/api/v1" + p, { headers: H })).json(),
    post: async (p, d) =>
      (
        await page.request.post(base + "/api/v1" + p, { headers: H, data: d })
      ).json(),
  };

  const key = "AT" + vp.width;
  if (!(await api.get("/projects")).some((p) => p.key === key))
    await api.post("/projects", { name: "Who picks up " + vp.width, key });
  const issue = await api.post("/issues", {
    project: key,
    title: "Who picks up",
  });
  ok(!issue.agent, `${tag}: a new issue is open to anyone (agent is empty)`);
  const stored = async (k, want) => {
    let got;
    for (let i = 0; i < 25; i++) {
      got = (await api.get("/issues/" + k)).agent ?? "";
      if (got === want) break;
      await page.waitForTimeout(200);
    }
    return got;
  };

  await page.goto(`${base}/issue/${issue.key}`);
  const scope = (root) => ({
    lock: () =>
      root
        .getByRole("button", { name: "Humans only", exact: true })
        .locator("visible=true"),
    assignees: () =>
      root.getByTitle("Assignees").locator("visible=true").first(),
    pick: async (name) => {
      await root
        .getByTitle("Assignees")
        .locator("visible=true")
        .first()
        .click();
      await page
        .getByRole("option", {
          name: name instanceof RegExp ? name : new RegExp("^" + name + "\\b"),
        })
        .last()
        .click();
      await page.keyboard.press("Escape");
    },
  });
  const who = scope(page);
  const pressed = async (r = who) =>
    (await r.lock().getAttribute("aria-pressed")) === "true";
  await who.lock().waitFor();
  ok(!(await pressed()), `${tag}: Humans only is off by default`);
  ok(
    (await page
      .getByTitle(/^For agents: /)
      .locator("visible=true")
      .count()) === 0,
    `${tag}: no agent shown on the assignee face by default`,
  );
  await page.screenshot({ path: `${out}/${tag}-anyone.png` });

  await who.lock().click();
  ok(
    (await stored(issue.key, "none")) === "none",
    `${tag}: one click makes it humans only (stored none)`,
  );
  ok(await pressed(), `${tag}: Humans only is on`);
  await page.screenshot({ path: `${out}/${tag}-humans.png` });

  await who.pick(/Robin Ames \(me\)/);
  await who.lock().waitFor({ state: "hidden" });
  ok(
    (await stored(issue.key, "none")) === "none",
    `${tag}: a person on a humans-only task hides the lock and keeps it`,
  );
  await who.pick(/Robin Ames \(me\)/);
  await who.lock().waitFor();
  ok(
    await pressed(),
    `${tag}: ...and taking the person off shows the lock again`,
  );

  await who.pick("Any agent");
  await page
    .getByTitle("For agents: any")
    .locator("visible=true")
    .first()
    .waitFor();
  ok(
    (await stored(issue.key, "any")) === "any",
    `${tag}: Any agent in the assignee list delegates it (stored any)`,
  );
  ok(
    (await who.lock().count()) === 0,
    `${tag}: the lock hides while an agent is on it`,
  );
  await page.screenshot({ path: `${out}/${tag}-agents.png` });

  await who.pick("sonnet");
  await page
    .getByTitle(/^For agents: sonnet/)
    .locator("visible=true")
    .first()
    .waitFor();
  ok(
    (await stored(issue.key, "sonnet")) === "sonnet",
    `${tag}: the list picks a specific agent`,
  );
  await page.screenshot({ path: `${out}/${tag}-specific.png` });

  await who.pick("sonnet");
  await page.waitForFunction(
    () => !document.querySelector('[title^="For agents: "]'),
  );
  ok(
    (await stored(issue.key, "")) === "",
    `${tag}: picking it again goes back to Anyone (stored empty)`,
  );
  await who.lock().waitFor();

  const find = async (q) =>
    (
      (await api.get("/issues?q=" + encodeURIComponent(`project:${key} ${q}`)))
        .issues ?? []
    ).map((i) => i.key);
  ok(
    (await find("is:anyone")).includes(issue.key),
    `${tag}: is:anyone finds it`,
  );
  await who.lock().click();
  await stored(issue.key, "none");
  ok(
    (await find("is:human")).includes(issue.key),
    `${tag}: is:human finds a humans-only issue`,
  );
  ok(
    !(await find("is:anyone")).includes(issue.key),
    `${tag}: ...and is:anyone doesn't`,
  );

  await page.goto(`${base}/p/${key}/list`);
  await page.getByText(issue.title).first().waitFor();
  await page.keyboard.press("c");
  const dlg = page.getByRole("dialog");
  const dwho = scope(dlg);
  await dwho.lock().waitFor();
  ok(
    !(await pressed(dwho)),
    `${tag}: the new-issue dialog starts open to anyone`,
  );
  await dwho.lock().click();
  ok(await pressed(dwho), `${tag}: the dialog takes Humans only in one click`);
  await page.screenshot({ path: `${out}/${tag}-dialog.png` });
  await page.getByLabel("Issue title").fill("Made humans only");
  await page.getByRole("button", { name: "Create issue" }).click();
  let madeKey;
  for (let i = 0; i < 25 && !madeKey; i++) {
    madeKey = ((
      await api.get(
        "/issues?q=" + encodeURIComponent(`project:${key} "Made humans only"`),
      )
    ).issues ?? [])[0]?.key;
    if (!madeKey) await page.waitForTimeout(200);
  }
  ok(
    !!madeKey && (await stored(madeKey, "none")) === "none",
    `${tag}: created from the dialog as humans only`,
  );
  await dlg.waitFor({ state: "hidden" });
  await page.keyboard.press("c");
  await dwho.lock().waitFor();
  ok(
    !(await pressed(dwho)),
    `${tag}: and the next new issue starts open to anyone again`,
  );
  await dwho.pick("Any agent");
  await page.getByLabel("Issue title").fill("Made for agents");
  await page.getByRole("button", { name: "Create issue" }).click();
  let agentKey;
  for (let i = 0; i < 25 && !agentKey; i++) {
    agentKey = ((
      await api.get(
        "/issues?q=" + encodeURIComponent(`project:${key} "Made for agents"`),
      )
    ).issues ?? [])[0]?.key;
    if (!agentKey) await page.waitForTimeout(200);
  }
  ok(
    !!agentKey && (await stored(agentKey, "any")) === "any",
    `${tag}: created from the dialog for any agent`,
  );

  const selectIt = async () => {
    await page.goto(`${base}/p/${key}/list`);
    await page.getByText("Made for agents").first().waitFor();
    await page.keyboard.press("j");
    while (
      !(await page.evaluate(() =>
        document.activeElement?.textContent?.includes("Made for agents"),
      ))
    )
      await page.keyboard.press("j");
    await page.keyboard.press("x");
    await page.getByText("1 selected").waitFor();
  };
  const bulkBtn = (name) =>
    page.getByRole("button", { name, exact: true }).locator("visible=true");
  await selectIt();
  await bulkBtn("Humans only").waitFor();
  await page.screenshot({ path: `${out}/${tag}-bulk.png` });
  await bulkBtn("Assign").click();
  await page
    .getByRole("option", { name: /^sonnet\b/ })
    .last()
    .click();
  ok(
    (await stored(agentKey, "sonnet")) === "sonnet",
    `${tag}: the bulk Assign list hands the selection to an agent`,
  );
  await selectIt();
  await bulkBtn("Humans only").click();
  await page
    .getByRole("option", { name: /^Humans only/ })
    .last()
    .click();
  ok(
    (await stored(agentKey, "none")) === "none",
    `${tag}: the bulk bar locks the selection to humans`,
  );
  await selectIt();
  await bulkBtn("Humans only").click();
  await page
    .getByRole("option", { name: /^Agents may take them/ })
    .last()
    .click();
  ok(
    (await stored(agentKey, "")) === "",
    `${tag}: ...and unlocks it (stored empty)`,
  );

  await ctx.close();
}
await browser.close();
console.log(errors.length ? `${errors.length} FAILURES` : "no errors");
process.exit(errors.length ? 1 : 0);
