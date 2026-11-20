// Admin → Email (WP-7): an admin sets the SMTP server; the email automation
// action is offered only once it is set; the password is never shown back;
// turning email off takes the action away again. Desktop and 390px.
// Run after 01-signup-flow (user robin, an admin).
import { chromium } from "playwright";
import fs from "node:fs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };
const browser = await chromium.launch();

async function login(ctx) {
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

for (const [width, scheme] of [
  [1280, "light"],
  [390, "dark"],
]) {
  const tag = `w${width}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await login(ctx);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  const api = async (path) =>
    (await page.request.get(base + "/api/v1" + path, { headers: H })).json();
  const offersEmail = async () =>
    (await api("/automation-catalog")).actions.some((a) => a.name === "email");

  ok(!(await offersEmail()), `${tag}: no email action before SMTP is set`);
  await page.goto(base + "/admin?tab=email");
  await page.getByText("SMTP server").first().waitFor();
  ok(await page.getByText("Off", { exact: true }).isVisible(), `${tag}: Off`);
  ok(
    !(await page.getByRole("button", { name: "Send test email" }).count()),
    `${tag}: no test until set up`,
  );

  await page.getByLabel("Host").fill("smtp.example.com");
  await page.getByLabel("Security").click();
  await page.getByRole("option", { name: "TLS", exact: true }).click();
  ok(
    (await page.getByLabel("Port").inputValue()) === "465",
    `${tag}: TLS moves the port to 465`,
  );
  await page.getByLabel("Username").fill("waypoint");
  await page.getByLabel("Password").fill("s3cret-pass");
  await page.getByLabel("From").fill("Waypoint <wp@example.com>");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("On", { exact: true }).waitFor();
  ok(
    (await page.getByLabel("Password").inputValue()) === "",
    `${tag}: password not shown back`,
  );
  ok(
    await page.getByText("A password is set").isVisible(),
    `${tag}: says a password is set`,
  );
  const saved = await api("/admin/email");
  ok(
    saved.configured && saved.has_password && !saved.password,
    `${tag}: API hides the password`,
  );
  ok(await offersEmail(), `${tag}: email action offered once set`);
  ok(
    await page.getByRole("button", { name: "Send test email" }).isVisible(),
    `${tag}: test form shown`,
  );
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  );
  ok(overflow <= 0, `${tag}: no horizontal scroll (${overflow}px)`);
  await page.screenshot({ path: `${out}/${tag}-email.png`, fullPage: true });

  await page.getByRole("button", { name: "Turn email off" }).click();
  await page.getByText("Off", { exact: true }).waitFor();
  ok(!(await offersEmail()), `${tag}: turning it off removes the action`);
  ok(
    (await page.getByLabel("Host").inputValue()) === "",
    `${tag}: settings cleared`,
  );
  await ctx.close();
}

await browser.close();
if (errors.length) {
  console.log(`${errors.length} failure(s)`);
  process.exit(1);
}
