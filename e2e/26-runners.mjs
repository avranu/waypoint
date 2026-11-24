// Runners (WP-D17), in a real browser at desktop and phone widths, light and
// dark: a real `wp runner` (the built CLI) connects to the server with a
// person's token; Settings → Runners shows it online with its labels,
// allowed folders and the coding-agent CLIs found on it (signed in or
// out); a command run from the page streams its output live onto the job
// page; a failed job, an applied change and a cancelled job show their
// results; the admin sees everyone's runners; removing one stops it.
// Run after 01-signup-flow (user robin, an admin). Needs `make build` (the
// runner is bin/cli/wp-<os>-<arch>) and git.
import { chromium } from "playwright";
import { spawn, execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { looks } from "./looks.mjs";
const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const errors = [];
const fail = (m) => (console.log("FAIL", m), errors.push(m));
const ok = (c, m) => (c ? console.log("ok  ", m) : fail(m));
const H = { "X-Waypoint-Client": "web" };

// ---- the runner: the real binary, with stand-ins for two agent CLIs ----
const arch = { x64: "amd64", arm64: "arm64" }[process.arch];
const wp =
  process.env.WP_BIN ||
  path.resolve(`../bin/cli/wp-${process.platform}-${arch}`);
if (!fs.existsSync(wp)) {
  console.log(`FAIL no wp binary at ${wp}; run make build first`);
  process.exit(1);
}
const root = fs.mkdtempSync(path.join(os.tmpdir(), "wp-e2e-runner-"));
const work = path.join(root, "projects");
const repo = path.join(work, "app");
const stubs = path.join(root, "bin");
fs.mkdirSync(repo, { recursive: true });
fs.mkdirSync(stubs);
const script = (name, body) => {
  fs.writeFileSync(path.join(stubs, name), `#!/bin/sh\n${body}\n`, {
    mode: 0o755,
  });
};
script(
  "claude",
  `case "$1" in --version) echo "2.1.296 (Claude Code)";; auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';; esac`,
);
script(
  "codex",
  `case "$1" in --version) echo "codex-cli 0.5.1";; login) echo "Not logged in"; exit 1;; esac`,
);
const git = (...a) =>
  execFileSync("git", a, {
    cwd: repo,
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" },
  })
    .toString()
    .trim();
git("init", "-q", "-b", "main");
git("config", "user.name", "Repo Owner");
git("config", "user.email", "owner@example.com");
fs.writeFileSync(path.join(repo, "hello.txt"), "hello\n");
git("add", "-A");
git("commit", "-q", "-m", "first");

let runner = null;
let runnerLog = "";
function startRunner(token) {
  runner = spawn(
    wp,
    [
      "runner",
      "--name",
      "e2e-box",
      "--label",
      "e2e",
      "--allow",
      work,
      "--max-jobs",
      "2",
    ],
    {
      env: {
        PATH: `${stubs}:/usr/local/bin:/usr/bin:/bin`,
        HOME: root,
        WAYPOINT_CONFIG_DIR: path.join(root, "config"),
        WAYPOINT_SERVER: base,
        WAYPOINT_TOKEN: token,
      },
    },
  );
  runner.stdout.on("data", (d) => (runnerLog += d));
  runner.stderr.on("data", (d) => (runnerLog += d));
}

const browser = await chromium.launch();
let exited = null;

async function run(width, scheme, first, last) {
  const tag = `${width}/${scheme}`;
  const ctx = await browser.newContext({
    viewport: { width, height: 900 },
    colorScheme: scheme,
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", (e) => fail(`${tag} pageerror: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() === "error" && !/Failed to load resource/.test(m.text()))
      fail(`${tag} console: ${m.text().slice(0, 300)}`);
  });
  const call = (m, p, d) =>
    page.request.fetch(base + "/api/v1" + p, {
      method: m,
      headers: H,
      data: d,
    });
  const api = (m, p, d) =>
    call(m, p, d).then((r) => (r.status() === 204 ? null : r.json()));
  const shot = async (name) => {
    await page.waitForTimeout(300);
    const tall = await page.evaluate(() => {
      const panes = [...document.querySelectorAll(".overflow-y-auto")];
      panes.forEach((e) => e.scrollTo(0, 0));
      return Math.max(...panes.map((e) => e.scrollHeight));
    });
    await page.setViewportSize({
      width,
      height: Math.min(Math.max(900, tall + 120), 6000),
    });
    await page.screenshot({ path: `${out}/${name}-${width}-${scheme}.png` });
    await page.setViewportSize({ width, height: 900 });
  };
  const overflow = async (where) =>
    ok(
      !(await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
      )),
      `${tag} no sideways scroll (${where})`,
    );
  const settled = async (id, states, ms = 30000) => {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      const j = await api("GET", `/runner-jobs/${id}`);
      if (states.includes(j.state)) return j;
      await new Promise((r) => setTimeout(r, 300));
    }
    throw new Error(`job ${id} never reached ${states}`);
  };

  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill("robin");
  await page.getByLabel("Password").fill("correct horse battery");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");

  if (first) {
    await page.goto(`${base}/settings?tab=runners`);
    await page.getByText("No runners yet").waitFor();
    const cmd = (
      await page.locator("code", { hasText: "curl -fsSL" }).first().innerText()
    ).trim();
    ok(
      cmd === `curl -fsSL ${base}/setup | sh`,
      `${tag} the one command is "${cmd}"`,
    );
    const script = await (await fetch(`${base}/setup`)).text();
    ok(
      script.startsWith("#!/bin/sh") && script.includes("setup=1"),
      `${tag} /setup serves the setup script`,
    );
    await shot("runners-empty");
    await overflow("empty runners");

    const { token } = await api("POST", "/me/tokens", { name: "e2e runner" });
    startRunner(token);
    runner.on("exit", (code) => (exited = code));
    for (let n = 0; n < 60; n++) {
      const rs = await api("GET", "/runners");
      if (rs.length && rs[0].online) break;
      await new Promise((r) => setTimeout(r, 250));
    }
  }

  await page.goto(`${base}/settings?tab=runners`);
  const row = page.getByTestId("runner").first();
  await row.waitFor();
  ok(
    (await row.innerText()).includes("e2e-box"),
    `${tag} the runner is listed by name`,
  );
  await row.getByText(/^Online/).waitFor();
  ok(true, `${tag} it is online`);
  ok(
    (await row.getByText("e2e", { exact: true }).count()) === 1,
    `${tag} its label shows`,
  );
  ok(
    (await row.getByText(work).count()) === 1,
    `${tag} its allowed folder shows`,
  );
  await row.getByText("Claude Code").waitFor();
  ok(
    (await row.innerText()).includes("signed in"),
    `${tag} claude is signed in`,
  );
  ok(
    /Codex[\s\S]*signed out/.test(await row.innerText()),
    `${tag} codex is signed out, with a hint on hover`,
  );
  ok(
    (await row.getByRole("button", { name: /Run a command/ }).count()) === 1,
    `${tag} a command can be run`,
  );
  if (process.platform === "linux") {
    const cap = row.getByTestId("runner-capacity");
    await cap.waitFor();
    ok(
      /One job at a time|Up to \d+ heavy jobs? of 2/.test(
        await cap.innerText(),
      ),
      `${tag} it says how many jobs it takes at once: "${(await cap.innerText()).trim()}"`,
    );
  }
  await shot("runners");
  await overflow("runners");

  await page.getByRole("button", { name: "Connect a runner" }).click();
  await page.getByText("On the computer you want to connect, run:").waitFor();
  ok(
    await page.getByText(/Only you, or a rule you own/).isVisible(),
    `${tag} the panel says who can use a runner`,
  );
  await shot("runners-connect");
  await overflow("connect panel");
  await page.getByRole("button", { name: "Connect a runner" }).click();

  await row.getByRole("button", { name: /Run a command/ }).click();
  const dialog = page.getByRole("dialog");
  await dialog
    .getByLabel("Command")
    .fill("echo hello from the runner; sleep 1; echo and goodbye");
  await dialog.getByRole("button", { name: "Run", exact: true }).click();
  await page.waitForURL(/\/runner-jobs\/\d+$/);
  await page
    .getByTestId("job-log")
    .getByText("hello from the runner")
    .waitFor();
  ok(true, `${tag} output appears on the job page`);
  await page.getByText("Done", { exact: true }).first().waitFor();
  ok(
    (await page.getByTestId("job-log").innerText()).includes("and goodbye"),
    `${tag} the rest arrives, and the job is done`,
  );
  ok(
    (await page.getByText("exit code").count()) > 0,
    `${tag} the exit code shows`,
  );
  await shot("job-done");
  await overflow("job page");

  if (first) {
    const live = await api("POST", "/runner-jobs", {
      kind: "command",
      input: {
        command: "for i in 1 2 3 4 5; do echo tick $i; sleep 1; done",
        dir: work,
      },
    });
    await page.goto(`${base}/runner-jobs/${live.id}`);
    await page.getByTestId("job-log").getByText("tick 1").waitFor();
    ok(
      await page.getByText("Running", { exact: true }).first().isVisible(),
      `${tag} still running while its output shows`,
    );
    await shot("job-running");
    await page.getByText("Done", { exact: true }).first().waitFor();
    ok(
      (await page.getByTestId("job-log").innerText()).includes("tick 5"),
      `${tag} all the ticks arrive`,
    );

    const bad = await api("POST", "/runner-jobs", {
      kind: "command",
      input: { command: "echo oops >&2; exit 3", dir: repo },
    });
    await settled(bad.id, ["failed"]);
    await page.goto(`${base}/runner-jobs/${bad.id}`);
    await page.getByText("Failed", { exact: true }).first().waitFor();
    ok(
      (await page.getByText("exited with code 3").count()) > 0,
      `${tag} a failed job shows its exit code and error`,
    );
    ok(
      (await page.getByText("exit code").count()) > 0,
      `${tag} the exit code shows`,
    );
    await shot("job-failed");

    const blob = git("hash-object", "hello.txt");
    const applied = await api("POST", "/runner-jobs", {
      kind: "apply",
      input: {
        dir: repo,
        commit: "Apply the e2e change",
        files: [
          { path: "hello.txt", base_sha: blob, content: "hello, world\n" },
          { path: "docs/new.md", content: "# New\n" },
        ],
      },
    });
    await settled(applied.id, ["done"]);
    ok(
      git("log", "-1", "--format=%s") === "Apply the e2e change",
      `${tag} the change was committed in the repository`,
    );
    await page.goto(`${base}/runner-jobs/${applied.id}`);
    await page.getByText("2 files written").waitFor();
    ok(
      (await page.getByText("docs/new.md").count()) > 0,
      `${tag} the result lists the files`,
    );
    await shot("job-apply");
    const stale = await api("POST", "/runner-jobs", {
      kind: "apply",
      input: {
        dir: repo,
        files: [{ path: "hello.txt", base_sha: blob, content: "other\n" }],
      },
    });
    const staleDone = await settled(stale.id, ["failed"]);
    ok(
      /nothing was written/.test(staleDone.error),
      `${tag} a change made against an old file is refused`,
    );

    const slow = await api("POST", "/runner-jobs", {
      kind: "command",
      input: { command: "echo started; sleep 120", dir: work },
    });
    await page.goto(`${base}/runner-jobs/${slow.id}`);
    await page.getByTestId("job-log").getByText("started").waitFor();
    await page.getByRole("button", { name: "Cancel" }).click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "Cancel job" })
      .click();
    await page.getByText("Cancelled", { exact: true }).first().waitFor();
    ok(true, `${tag} a running job is cancelled from its page`);
    await shot("job-cancelled");
  }

  await page.goto(`${base}/settings?tab=runners`);
  const again = page.getByTestId("runner").first();
  await again.getByText("Recent jobs").waitFor();
  ok(
    (await again.locator('a[href^="/runner-jobs/"]').count()) >= 1,
    `${tag} recent jobs link to their pages`,
  );
  await shot("runners-jobs");

  await page.goto(`${base}/admin?tab=runners`);
  await page.getByText("Everyone's runners").waitFor();
  await page.getByTestId("runner").first().waitFor(); // count() does not wait, and the list loads after its heading.
  ok(
    (await page.getByTestId("runner").count()) >= 1,
    `${tag} Admin → Runners lists runners`,
  );
  ok(
    (await page.getByRole("button", { name: /Run a command/ }).count()) === 0,
    `${tag} the admin view runs nothing`,
  );
  await shot("runners-admin");

  if (last) {
    await page.goto(`${base}/settings?tab=runners`);
    await page.getByRole("button", { name: "Remove e2e-box" }).click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "Remove" })
      .click();
    await page.getByText("No runners yet").waitFor();
    ok(true, `${tag} the runner is gone from the list`);
    for (let n = 0; n < 100 && exited === null; n++)
      await new Promise((r) => setTimeout(r, 200));
    ok(
      exited === 0,
      `${tag} the runner process ended cleanly (exit ${exited})`,
    );
  }
  await ctx.close();
}

const variants = looks([
  [1440, "light"],
  [1440, "dark"],
  [390, "light"],
  [390, "dark"],
]);
try {
  for (const [i, [w, s]] of variants.entries()) {
    try {
      await run(w, s, i === 0, i === variants.length - 1);
    } catch (e) {
      fail(`${w}/${s}: ${e.message.split("\n")[0]}`);
    }
  }
} finally {
  if (runner && exited === null) runner.kill("SIGTERM");
  await browser.close();
  fs.rmSync(root, { recursive: true, force: true });
}
if (errors.length) {
  console.log(runnerLog.split("\n").slice(-25).join("\n"));
  console.log(`${errors.length} failures`);
  process.exit(1);
}
console.log("all ok");
