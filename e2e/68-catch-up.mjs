// Catch up (WP-245) in a real browser at desktop and phone widths: an
// agent's finished tasks show one at a time with its summary, its review
// screenshot (wp shot uploads with ?review=1) and where to try it; arrows
// move between them; Looks good reviews the close, Not finished reopens the
// task with a note, Report a problem files a related bug with a picture.
// Run after 01-signup-flow (user robin, an admin).
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

async function run(width, scheme) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error") fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d, headers = H, extra = {}) =>
    page.request.fetch(base + "/api/v1" + p, {
      method: m,
      headers,
      data: d,
      ...extra,
    });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = (name) =>
    page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
  const overflow = () =>
    page.evaluate(
      () => document.documentElement.scrollWidth > window.innerWidth + 1,
    );

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  const key = `CU${width}${scheme[0]}`.toUpperCase();
  await api("POST", "/projects", { name: `Catch up ${tag}`, key });
  const { token } = await api("POST", "/me/tokens", {
    name: `finisher ${tag}`,
    agent: "anthropic/claude-haiku-5-5",
  });
  const AG = { ...H, Authorization: `Bearer ${token}` };
  // A real picture to stand in for what wp shot would take.
  await page.goto(`${base}/p/${key}/board`);
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(300);
  const picture = await page.screenshot();

  const keys = [];
  for (const t of ["Add the page", "Fix the button", "Tidy the list"]) {
    const k = (
      await api("POST", "/issues", { project: key, title: `${t} ${tag}` })
    ).key;
    keys.push(k);
    await call(
      "POST",
      `/issues/${k}/comments`,
      { body: `**${t}**: done, see the board.` },
      AG,
    );
    const r = await call(
      "POST",
      `/issues/${k}/transition`,
      { status: "Done" },
      AG,
    );
    ok(r.ok(), `${tag} the agent finished ${k} (${r.status()})`);
  }
  const [k1, k2, k3] = keys;
  const up = await call(
    "POST",
    `/attachments?target=issue:${k1}&review=1`,
    undefined,
    AG,
    {
      multipart: {
        file: { name: "board.png", mimeType: "image/png", buffer: picture },
        caption: "Desktop · board",
      },
    },
  );
  ok(up.ok() && (await up.json()).review, `${tag} API: a review screenshot`);
  for (const url of [`/p/${key}/board`, "https://example.com/try"]) {
    const r = await call("POST", `/issues/${k1}/try-links`, { url }, AG);
    ok(r.ok(), `${tag} API: try link ${url} (${r.status()})`);
  }

  // ---- the slideshow ----
  await page.goto(`${base}/catch-up?project=${key}`);
  const item = page.getByTestId("catch-up-item");
  await item.getByText(`Add the page ${tag}`).waitFor();
  ok(
    await page.getByText("1 of 3").isVisible(),
    `${tag} counts the three finished tasks`,
  );
  ok(
    await item.getByText("done, see the board.").isVisible(),
    `${tag} shows the agent's summary`,
  );
  ok(
    await item.getByRole("link", { name: /example\.com\/try/ }).isVisible(),
    `${tag} a full try link opens`,
  );
  ok(
    await item.getByText(`/p/${key}/board`).isVisible(),
    `${tag} a path with no environment URL shows as text`,
  );
  const img = item.getByRole("img", { name: "Desktop · board" });
  await img.waitFor();
  await img.scrollIntoViewIfNeeded();
  await page.waitForFunction(
    (el) => el.complete && el.naturalWidth > 0,
    await img.elementHandle(),
  );
  ok(!(await overflow()), `${tag} catch up has no sideways scroll`);
  await shot("slide");
  await item.getByRole("button", { name: /Open Desktop · board/ }).click();
  await page.getByRole("dialog").waitFor();
  await page.keyboard.press("Escape");
  await page.getByRole("dialog").waitFor({ state: "hidden" });

  await page.keyboard.press("ArrowRight");
  await item.getByText(`Fix the button ${tag}`).waitFor();
  ok(await page.getByText("2 of 3").isVisible(), `${tag} → moves on`);
  await page.keyboard.press("ArrowLeft");
  await item.getByText(`Add the page ${tag}`).waitFor();

  // Looks good reviews the close; the next one shows in its place.
  await page.getByRole("button", { name: "Looks good" }).click();
  await item.getByText(`Fix the button ${tag}`).waitFor();
  ok(await page.getByText("1 of 2").isVisible(), `${tag} one fewer to see`);
  ok(
    !(await api("GET", `/issues/${k1}`)).close_unreviewed,
    `${tag} Looks good reviewed the close`,
  );

  // Not finished sends it back with a note.
  await page.getByRole("button", { name: "Not finished" }).click();
  const back = page.getByRole("dialog", { name: `Send ${k2} back` });
  await back.getByLabel("What’s missing").fill("The button is still blue.");
  await shot("not-finished");
  await back.getByRole("button", { name: "Reopen it" }).click();
  await item.getByText(`Tidy the list ${tag}`).waitFor();
  const d2 = await api("GET", `/issues/${k2}`);
  ok(d2.status.category === "todo", `${tag} Not finished reopened it`);
  const cs = await api("GET", `/issues/${k2}/comments`);
  const list = Array.isArray(cs) ? cs : (cs.items ?? cs.comments ?? []);
  ok(
    list.some((c) => c.body === "Not finished: The button is still blue."),
    `${tag} with the note as a comment`,
  );

  // Report a problem: a related bug with a picture; the task stays to review.
  await page.getByRole("button", { name: "Report a problem" }).click();
  const rep = page.getByRole("dialog", { name: `Report a problem with ${k3}` });
  await rep.getByLabel("What’s wrong").fill(`Rows overlap ${tag}\nOn a phone.`);
  await rep
    .locator("input[type=file]")
    .setInputFiles({
      name: "rows.png",
      mimeType: "image/png",
      buffer: picture,
    });
  await rep.getByRole("list", { name: "Pictures" }).getByRole("img").waitFor();
  await page.waitForTimeout(300);
  await shot("problem");
  ok(!(await overflow()), `${tag} the problem dialog has no sideways scroll`);
  await rep.getByRole("button", { name: "File it" }).click();
  const filed = page.getByText(/^Filed [A-Z0-9]+-\d+$/);
  await filed.waitFor();
  const bugKey = (await filed.innerText()).replace("Filed ", "");
  const bug = await api("GET", `/issues/${bugKey}`);
  ok(
    bug.type === "bug" && bug.title === `Rows overlap ${tag}`,
    `${tag} filed a bug titled from the note (${bug.type} ${bug.title})`,
  );
  ok(
    (bug.relations ?? []).some((r) => r.issue.key === k3),
    `${tag} related to the task`,
  );
  ok(
    (bug.attachments ?? []).some((a) => a.name === "rows.png"),
    `${tag} with the picture`,
  );
  ok(
    await item.getByText(`Tidy the list ${tag}`).isVisible(),
    `${tag} the task still waits for a look`,
  );

  await page.getByRole("button", { name: "Looks good" }).click();
  await page.getByText("All caught up").waitFor();
  await shot("caught-up");

  // ---- the way in: Waiting on you and the nav ----
  await call("POST", `/issues/${k2}/transition`, { status: "Done" }, AG);
  await page.goto(base + "/inbox");
  const row = page
    .getByRole("region", { name: "Waiting on you" })
    .getByRole("link", { name: /agents finished, not looked at yet/ });
  await row.waitFor();
  await row.click();
  await page.waitForURL(/\/catch-up$/);
  await page.getByRole("heading", { name: "Catch up" }).waitFor();
  await call("POST", `/review/closes`, { issues: [k2] });
  await ctx.close();
}

for (const [w, s] of looks([
  [1280, "light"],
  [390, "dark"],
])) {
  try {
    await run(w, s);
  } catch (e) {
    fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
  }
}
await browser.close();
if (errors.length) {
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
