// UI polish checks (WP-680): deterministic, DOM-only checks for categories of
// polish problems on every page and tab of the app, so nobody has to look at
// each component by eye. Run after 01-signup-flow: robin (an admin) creates
// the suite's own admin, uicheck, and everything else runs as that one.
//
// Categories (each finding is keyed "category | route | element", digits as #):
//   overflow      the page (or the main scroller) scrolls sideways, and which
//                 elements stick out of it, not counting ones a scroller clips
//   clipped       text cut off with no ellipsis (overflow hidden/clip and
//                 content bigger than the box), and an input whose placeholder
//                 is wider than the field
//   leak          visible text with markup or code leaking: [[ ]], [object
//                 Object], undefined, NaN, null, {braces}, **, &amp;
//   duplicate     the same sentence twice in one card/row, or a heading or
//                 summary repeated by the first line under it
//   overlap       interactive elements, badges or icons whose boxes overlap
//   empty-section a heading with nothing under it
//   stuck-status  "Saving…" / "Loading…" still showing once the page is idle
//   wide          at 1920 wide the content fills under 55% of the main area
//   dead-avatar   an avatar (data-avatar, ui/misc.tsx) outside any button or
//                 link: in the app's chrome (header, sidebar, top 64px) it
//                 looks clickable and isn't; elsewhere one with no title
//                 carries no information
//
// Routes come from web/src/router.tsx (every createRoute path joined to its
// parent's), so a new page is covered without editing this file; $params are
// filled from the seeded data below (PARAMS), and a route whose param has no
// seed is printed as skipped. Tabs: the ids of the TABS arrays in Settings,
// Admin and ProjectSettings and the AgentsTab union in agents/search.ts are
// read from source (TAB_SOURCES). If one of those moves, edit TAB_SOURCES.
//
// Looks: dark 1440x900 and light 390x844 (E2E_LOOKS=one keeps the phone,
// where overflow shows), and 1920x1080 for the wide check only.
//
// The ratchet: e2e/ui-checks-baseline.json maps known finding keys to a short
// note. The suite fails on findings that are not in it and prints (without
// failing) baseline entries no longer seen, so the baseline only shrinks.
//   UI_CHECKS_WRITE=1 node 79-ui-checks.mjs ...   rewrites the baseline from
//   the current findings, keeping existing notes (run.sh: UI_CHECKS_WRITE=1
//   ./run.sh 79). All findings are also written to <out>/ui-checks.json.
import { chromium } from "playwright";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { everyLook, looks } from "./looks.mjs";

const [base, out] = process.argv.slice(2);
fs.mkdirSync(out, { recursive: true });
const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.join(here, "..");
const baselineFile = path.join(here, "ui-checks-baseline.json");
const H = { "X-Waypoint-Client": "web" };
const KEY = "UIC";
const CONCURRENCY = 5;

const read = (f) => fs.readFileSync(path.join(root, "web/src", f), "utf8");
function routesFromSource() {
  const src = read("router.tsx");
  const defs = new Map();
  const parts = src.split(/\nconst (\w+) = createRoute\(/).slice(1);
  for (let i = 0; i < parts.length; i += 2) {
    const body = parts[i + 1].split(/\n(?:const|export|function) /)[0];
    const parent = /getParentRoute: \(\) => (\w+)/.exec(body)?.[1];
    const p = /\n\s+path: '([^']*)'/.exec(body)?.[1];
    const id = /\n\s+id: '([^']*)'/.exec(body)?.[1];
    defs.set(parts[i], { parent, seg: p ?? "", isId: !p && !!id });
  }
  const full = (n) => {
    const d = defs.get(n);
    if (!d) return "";
    const up = d.parent ? full(d.parent) : "";
    return d.isId ? up : (up + d.seg).replace(/\/\/+/g, "/");
  };
  const all = [...defs.keys()].map((n) => ({ name: n, path: full(n) || "/" }));
  return [...new Set(all.map((r) => r.path))]
    .filter((p) => !["/login", "/invite/$token", "/reset/$token", "/device"].includes(p))
    .sort();
}
const tabIds = (file, re) => [...read(file).matchAll(re)].map((m) => m[1]);
const TAB_SOURCES = {
  "/settings": () => tabIds("pages/Settings.tsx", /\{ id: '(\w+)', label/g),
  "/admin": () => tabIds("pages/Admin.tsx", /\{ id: '(\w+)', label/g),
  "/p/$key/settings": () => tabIds("pages/ProjectSettings.tsx", /\{ id: '(\w+)', label: '[^']*', icon/g),
  "/agents": () => {
    const m = /export type AgentsTab =([^]*?)\n\n/.exec(read("pages/agents/search.ts"));
    return [...m[1].matchAll(/'(\w+)'/g)].map((x) => x[1]);
  },
};

function inPage({ category }) {
  const cats = new Set(category);
  const W = innerWidth;
  const found = [];
  const norm = (s) => s.replace(/\s+/g, " ").trim();
  const digits = (s) => s.replace(/\d+/g, "#");
  const desc = (el) => {
    const label = el.getAttribute("aria-label") || el.getAttribute("title") || "";
    const txt = norm(el.innerText ?? el.textContent ?? "");
    const what = label || txt.slice(0, 40) || el.getAttribute("placeholder") || el.getAttribute("name") || "";
    return digits(`${el.tagName.toLowerCase()}${what ? " " + what.slice(0, 40) : ""}`);
  };
  const add = (c, el, detail, d) => found.push({ category: c, desc: d ?? desc(el), detail });

  const SKIP = "svg,script,style,noscript,[aria-hidden=true],.sr-only,[data-ui-check-ignore],.cm-editor,.cm-content,pre,textarea,[contenteditable=true],canvas,iframe";
  const visible = (el) => {
    if (!el.isConnected || el.closest("svg,[aria-hidden=true],.sr-only")) return false;
    if (!el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })) return false;
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) return false;
    const cs = getComputedStyle(el);
    if (cs.clip === "rect(0px, 0px, 0px, 0px)" || cs.clipPath === "inset(50%)") return false;
    return true;
  };
  const clipRects = (el) => {
    const out = [];
    for (let a = el.parentElement; a && a !== document.documentElement; a = a.parentElement) {
      const cs = getComputedStyle(a);
      if (cs.overflowX !== "visible" || cs.overflowY !== "visible") out.push(a.getBoundingClientRect());
    }
    return out;
  };
  const shown = (el, r = el.getBoundingClientRect()) =>
    visible(el) && clipRects(el).every((c) => r.right > c.left + 1 && r.left < c.right - 1 && r.bottom > c.top + 1 && r.top < c.bottom - 1);
  const all = [...document.body.querySelectorAll("*")];
  const body = document.querySelector("main") ?? document.body;

  if (cats.has("overflow")) {
    const scrollers = [document.documentElement, document.querySelector("main")].filter(Boolean);
    for (const s of scrollers) {
      const name = s === document.documentElement ? "page" : "main";
      if (s.scrollWidth > s.clientWidth + 1) add("overflow", s, `${name} scrolls sideways by ${s.scrollWidth - s.clientWidth}px`, `${name} scrolls sideways`);
      const box = s === document.documentElement ? { left: 0, right: W } : s.getBoundingClientRect();
      const out = new Set();
      for (const el of all) {
        if (!s.contains(el) || el === s || el.closest(SKIP) || !visible(el)) continue;
        const r = el.getBoundingClientRect();
        if (r.right <= box.right + 1 && r.left >= box.left - 1) continue;
        let contained = false;
        for (let a = el.parentElement; a && a !== s; a = a.parentElement) {
          const cs = getComputedStyle(a);
          if (cs.overflowX !== "visible") { contained = true; break; }
          if (cs.position === "fixed") { contained = true; break; }
        }
        if (contained || getComputedStyle(el).position === "fixed") continue;
        if (r.left >= box.right - 1 || r.right <= box.left + 1) continue; // Skip text entirely off the box: hidden slide-overs sit off-screen.
        out.add(el);
      }
      for (const el of out) {
        if (el.parentElement && out.has(el.parentElement)) continue;
        add("overflow", el, `${name}: ${desc(el)} ends ${Math.round(el.getBoundingClientRect().right - box.right)}px past the edge`);
      }
    }
  }

  if (cats.has("clipped")) {
    for (const el of all) {
      if (el.closest(SKIP) && !el.matches("input,textarea")) continue;
      if (el.matches("input")) {
        if (!el.placeholder || el.value || !visible(el) || !/^(text|search|email|url|tel|password|number|)$/.test(el.type)) continue;
        const cs = getComputedStyle(el);
        const c = document.createElement("canvas").getContext("2d");
        c.font = `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
        const avail = el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
        const need = c.measureText(el.placeholder).width;
        if (need > avail + 1) add("clipped", el, `placeholder needs ${Math.round(need)}px, field has ${Math.round(avail)}px`, digits(`input placeholder "${el.placeholder.slice(0, 40)}"`));
        continue;
      }
      if (el.matches("textarea")) continue;
      const cs = getComputedStyle(el);
      const hides = (v) => v === "hidden" || v === "clip";
      if (!hides(cs.overflowX) && !hides(cs.overflowY)) continue;
      if (cs.textOverflow === "ellipsis" || cs.webkitLineClamp !== "none" || cs.getPropertyValue("-webkit-line-clamp") !== "none") continue;
      if (!visible(el)) continue;
      // Only an element's own text counts; a layout box may clip its children by design.
      const own = [...el.childNodes].some((n) => n.nodeType === 3 && n.textContent.trim().length > 1);
      if (!own) continue;
      const x = hides(cs.overflowX) && el.scrollWidth > el.clientWidth + 1;
      const y = hides(cs.overflowY) && el.scrollHeight > el.clientHeight + 1;
      if (!x && !y) continue;
      // Skip near-zero-height regions: a collapsed toggle hides its content by design.
      if (el.clientHeight < 4 && y) continue;
      add("clipped", el, `${x ? `content ${el.scrollWidth}px in ${el.clientWidth}px` : `content ${el.scrollHeight}px in ${el.clientHeight}px`}: ${norm(el.textContent).slice(0, 60)}`);
    }
  }

  if (cats.has("leak")) {
    const re = [
      [/\[\[|\]\]/, "wikilink brackets"],
      [/\[object Object\]/, "object"],
      [/\bundefined\b/, "undefined"],
      [/\bNaN\b/, "NaN"],
      [/\bnull\b/, "null"],
      [/\{\{|\}\}|\{[A-Za-z_.]+\}|\{\s*\}/, "braces"],
      [/(?<![/*])\*\*(?![/*])/, "markdown bold"], // Matches a markdown ** only; the lookarounds skip glob text such as release/** and **/x.
      [/&(amp|lt|gt|quot|#\d+);/, "html entity"],
      [/\$\{/, "template"],
    ];
    const tw = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = tw.nextNode(); n; n = tw.nextNode()) {
      const t = n.textContent;
      if (!t.trim()) continue;
      const el = n.parentElement;
      if (!el || el.closest(`${SKIP},code,kbd,samp,input,option`) || !visible(el)) continue;
      for (const [r, what] of re) {
        const m = r.exec(t.replace(/\[\[links?\]\]/g, ""));
        if (!m) continue;
        const at = Math.max(0, m.index - 15);
        add("leak", el, `${what}: ${norm(t).slice(0, 80)}`, digits(`${el.tagName.toLowerCase()} ${what} "${t.slice(at, at + 40).trim()}"`));
      }
    }
  }

  if (cats.has("duplicate")) {
    const stop = /[^\p{L}\p{N} ]/gu;
    const key = (s) => norm(s).toLowerCase().replace(stop, "").replace(/\s+/g, " ").trim();
    const words = (s) => (s ? s.split(" ").length : 0);
    const INLINE = new Set(["SPAN", "A", "B", "STRONG", "EM", "I", "CODE", "SMALL", "TIME", "MARK", "ABBR", "BR"]);
    const blocks = [];
    for (const el of all) {
      if (el.closest(SKIP) || el.closest("button,[role=button],[role=tab],[role=menuitem],[role=option]")) continue;
      if (![...el.children].every((c) => INLINE.has(c.tagName))) continue;
      const k = key(el.innerText ?? "");
      if (words(k) >= 3 && visible(el)) blocks.push({ el, k });
    }
    const scopeSel = "li,tr,article,section,details,[role=listitem],[role=row],.rounded-lg,.rounded-xl,.rounded-md";
    const byScope = new Map();
    for (const b of blocks) {
      const s = b.el.closest(scopeSel);
      if (!s) continue;
      if (!byScope.has(s)) byScope.set(s, []);
      byScope.get(s).push(b);
    }
    // Sibling rows built alike may repeat text legitimately, e.g. two environments both down since the same time.
    const siblingRows = (a, b) => {
      let x = a;
      while (x.parentElement && !x.parentElement.contains(b)) x = x.parentElement;
      let y = b;
      while (y.parentElement && y.parentElement !== x.parentElement) y = y.parentElement;
      return x !== y && x.tagName === y.tagName && x.className === y.className;
    };
    const seen = new Set();
    for (const list of byScope.values()) {
      const m = new Map();
      for (const b of list) {
        const prev = m.get(b.k);
        if (prev && !prev.el.contains(b.el) && !b.el.contains(prev.el) && !siblingRows(prev.el, b.el)) {
          const id = b.k;
          if (!seen.has(id + "|" + b.el.closest(scopeSel).tagName)) {
            seen.add(id + "|" + b.el.closest(scopeSel).tagName);
            add("duplicate", b.el, `"${b.k.slice(0, 70)}" twice in one card`, digits(`${b.el.tagName.toLowerCase()} "${b.k.slice(0, 40)}"`));
          }
        } else m.set(b.k, b);
      }
    }
    for (const h of all) {
      if (!h.matches("h1,h2,h3,h4,h5,h6,summary") || h.closest(SKIP) || !visible(h)) continue;
      const hk = key(h.innerText);
      if (words(hk) < 2) continue;
      let next = h.matches("summary") ? h.parentElement.querySelector(":scope > :not(summary)") : h.nextElementSibling;
      if (!next && !h.matches("summary")) next = h.parentElement?.nextElementSibling;
      if (!next || !visible(next)) continue;
      const nk = key(next.innerText ?? "");
      if (nk === hk || (nk.startsWith(hk + " ") && words(hk) >= 3))
        add("duplicate", h, `heading "${hk.slice(0, 50)}" repeated right under it`, digits(`${h.tagName.toLowerCase()} "${hk.slice(0, 40)}" repeated below`));
    }
  }

  if (cats.has("overlap")) {
    const ISEL = "a[href],button,input:not([type=hidden]),select,textarea,[role=button],[role=tab],[role=menuitem],[role=checkbox],[role=switch],[role=combobox],[tabindex='0']";
    const nodes = [];
    for (const el of all) {
      if (el.closest("[aria-hidden=true],.sr-only,.cm-editor")) continue;
      const r0 = el.getBoundingClientRect();
      const small = r0.height <= 28 && r0.width <= 40;
      const badge = !el.matches("svg") && small && el.children.length === 0 && /^\d{1,3}\+?$/.test((el.textContent ?? "").trim()) && getComputedStyle(el).borderRadius !== "0px";
      const icon = el.matches("svg") && r0.width >= 10 && !el.closest(ISEL);
      const inter = el.matches(ISEL);
      if (!(inter || badge || icon)) continue;
      if (el.matches("input[type=checkbox],input[type=radio]") && getComputedStyle(el).opacity === "0") continue;
      const svgVisible = icon ? el.checkVisibility() && r0.width > 0 : visible(el);
      if (!svgVisible || !shown(el, r0)) continue;
      nodes.push({ el, r: r0, kind: inter ? "ctl" : badge ? "badge" : "icon" });
    }
    const seen = new Set();
    for (let i = 0; i < nodes.length; i++)
      for (let j = i + 1; j < nodes.length; j++) {
        const a = nodes[i], b = nodes[j];
        if (a.el.contains(b.el) || b.el.contains(a.el)) continue;
        if (a.kind === "icon" && b.kind === "icon") continue;
        const iw = Math.min(a.r.right, b.r.right) - Math.max(a.r.left, b.r.left);
        const ih = Math.min(a.r.bottom, b.r.bottom) - Math.max(a.r.top, b.r.top);
        if (iw <= 4 || ih <= 4) continue;
        const area = iw * ih;
        const minA = Math.min(a.r.width * a.r.height, b.r.width * b.r.height);
        if (area / minA < 0.2) continue;
        if (a.el.closest("label") && a.el.closest("label") === b.el.closest("label")) continue;
        const cx = (Math.max(a.r.left, b.r.left) + Math.min(a.r.right, b.r.right)) / 2;
        const cy = (Math.max(a.r.top, b.r.top) + Math.min(a.r.bottom, b.r.bottom)) / 2;
        const top = document.elementFromPoint(cx, cy);
        if (!top || !(a.el.contains(top) || b.el.contains(top) || top.contains(a.el) || top.contains(b.el))) continue;
        const layer = (el) => { for (let n = el; n && n !== document.body; n = n.parentElement) if (getComputedStyle(n).position === "fixed" || n.matches("[role=dialog]")) return n; return null; };
        if (layer(a.el) !== layer(b.el)) continue;
        const da = desc(a.el), db = desc(b.el);
        const k = [da, db].sort().join(" x ");
        if (seen.has(k)) continue;
        seen.add(k);
        add("overlap", a.el, `${da} overlaps ${db} by ${Math.round(iw)}x${Math.round(ih)}px at ${Math.round(Math.max(a.r.left, b.r.left))},${Math.round(Math.max(a.r.top, b.r.top))}`, k);
      }
  }

  if (cats.has("empty-section")) {
    const content = (s) => {
      if (!visible(s) && !s.matches("li,ul,ol,div,section,table")) return false;
      if (s.matches("h1,h2,h3,h4,h5,h6") || s.querySelector(":scope > h1,:scope > h2,:scope > h3,:scope > h4,:scope > h5,:scope > h6")) return null;
      if (!visible(s)) return false;
      // A pulsing skeleton means the section is still loading, so it is not counted as empty.
      if (s.matches(".animate-pulse") || s.querySelector(".animate-pulse")) return true;
      if (s.matches("button,a") || ([...s.querySelectorAll("*")].length < 8 && !norm(s.innerText ?? "") && !s.querySelector("img,input,textarea,select,table,canvas,video"))) return false;
      return !!norm(s.innerText ?? "") || !!s.querySelector("img,input,textarea,select,table,canvas,video");
    };
    for (const h of all) {
      if (!h.matches("h1,h2,h3,h4,h5,h6") || h.closest(SKIP + ",[role=dialog],[role=menu]") || !visible(h) || !norm(h.innerText)) continue;
      if (h.matches("h1") && h.closest("header")) continue;
      let c = h, ok = false;
      for (let d = 0; d < 4 && !ok && c; d++) {
        for (let s = c.nextElementSibling; s; s = s.nextElementSibling) {
          const v = content(s);
          if (v === null) break;
          if (v) { ok = true; break; }
        }
        const p = c.parentElement;
        if (ok || !p || p === body || p === document.body || p.querySelectorAll("h1,h2,h3,h4,h5,h6").length !== 1) break;
        c = p;
      }
      if (!ok) add("empty-section", h, `nothing under "${norm(h.innerText).slice(0, 50)}"`);
    }
  }

  if (cats.has("stuck-status")) {
    const re = /(^|\s)(Saving|Loading|Uploading|Searching|Sending)(…|\.\.\.)(\s|$)/;
    const tw = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = tw.nextNode(); n; n = tw.nextNode()) {
      const el = n.parentElement;
      if (!el || !re.test(n.textContent) || el.closest("svg,script,style,[aria-hidden=true],.sr-only,option") || !visible(el)) continue;
      add("stuck-status", el, `still showing "${norm(n.textContent).slice(0, 50)}"`);
    }
  }

  if (cats.has("wide")) {
    const m = document.querySelector("main");
    if (m && W >= 1700) {
      const mr = m.getBoundingClientRect();
      let l = Infinity, r = -Infinity;
      for (const el of m.querySelectorAll("*")) {
        if (el.closest("svg") && !el.matches("svg")) continue;
        const leaf = el.children.length === 0 || el.matches("img,svg,canvas,input,textarea,select,button,table");
        if (!leaf || !visible(el)) continue;
        if (!norm(el.textContent ?? "") && !el.matches("img,svg,canvas,input,textarea,select,table")) continue;
        const b = el.getBoundingClientRect();
        if (b.width > mr.width * 0.98 && !norm(el.textContent ?? "")) continue;
        l = Math.min(l, b.left); r = Math.max(r, b.right);
      }
      const used = r > l ? (r - l) / mr.width : 0;
      if (used > 0 && used < 0.55) add("wide", m, `content spans ${Math.round(used * 100)}% of the ${Math.round(mr.width)}px main area`, "main content under 55% of width");
    }
  }

  if (cats.has("dead-avatar")) {
    for (const el of document.querySelectorAll("[data-avatar]")) {
      if (!visible(el)) continue;
      if (el.closest("button,a[href],[role=button],[role=link],[role=menuitem],[role=tab],[role=option],summary,label,[tabindex='0']")) continue;
      const r = el.getBoundingClientRect();
      const chrome = el.closest("nav") || r.top < 64;
      const where = `at ${Math.round(r.left)},${Math.round(r.top)} ${Math.round(r.width)}px`;
      if (chrome) add("dead-avatar", el, `avatar in the page chrome is not a control: ${el.title || "no title"} ${where}; parent ${desc(el.parentElement)}`, `avatar in chrome ${el.title || ""}`.trim());
      else if (!el.title && !el.alt && !el.getAttribute("aria-label")) add("dead-avatar", el, `avatar with no title or control ${where}; parent ${desc(el.parentElement)}`, `avatar without title in ${desc(el.parentElement)}`);
    }
  }
  return found;
}

const browser = await chromium.launch();
const findings = new Map();
const skipped = [];
const errors = [];
let visited = 0;

const groupRoute = (f, route) => (f.category === "dead-avatar" && route.startsWith("/p/$key") ? "/p/$key/*" : route);
const keyOf = (f, route) => `${f.category} | ${route} | ${f.desc}`;
function record(route, look, list) {
  for (const f of list) {
    const rt = groupRoute(f, route);
    const k = keyOf(f, rt);
    const e = findings.get(k) ?? { category: f.category, route: rt, desc: f.desc, details: [], looks: [] };
    if (!e.looks.includes(look)) e.looks.push(look);
    if (e.details.length < 3 && !e.details.includes(f.detail)) e.details.push(f.detail);
    findings.set(k, e);
  }
}

// The checks sign in as an admin of their own, so the pages that list
// everything a person sees (inbox, agents, environments, settings) show this
// suite's data, not whatever the suites before it in the shard left there.
const USER = { username: "uicheck", password: "ui checks long password" };
async function login(ctx, who = USER) {
  const page = await ctx.newPage();
  await page.goto(base + "/login");
  await page.getByLabel("Username or email").fill(who.username);
  await page.getByLabel("Password").fill(who.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(base + "/");
  return page;
}

// Seed through the API once, so pages show content and not just empty states.
async function seed(page) {
  const call = (m, p, d, extra = {}) => page.request.fetch(base + "/api/v1" + p, { method: m, headers: H, data: d, ...extra });
  const api = async (m, p, d) => {
    const r = await call(m, p, d);
    const t = await r.text();
    if (r.status() >= 300) throw new Error(`${m} ${p}: ${r.status()} ${t.slice(0, 200)}`);
    try { return JSON.parse(t); } catch { return t; }
  };
  const have = (await api("GET", "/projects")).some((p) => p.key === KEY);
  const ids = { key: KEY };
  if (!have) await api("POST", "/projects", { name: "UI checks", key: KEY });
  const long = Array.from({ length: 8 }, (_, i) => `Paragraph ${i + 1}: the board shows every task as a card with a title, a status, who it is assigned to and when it last changed, and it keeps showing them as the list grows well past one screen.`).join("\n\n");
  const issues = await api("GET", `/issues?project=${KEY}`).catch(() => []);
  let list = Array.isArray(issues) ? issues : (issues.issues ?? []);
  if (list.length < 3) {
    const a = await api("POST", "/issues", { project: KEY, title: "Show the route on the map with a very long title that keeps going so we can see how a long title wraps on a phone screen", description: `${long}\n\n- first item\n- second item\n\n\`\`\`js\nconst answer = 42\n\`\`\`\n\nSee the [[UI checks glossary term]] and https://example.com/a/very/long/url/that/does/not/break/anywhere/at/all/in/the/text/and/goes/on/and/on` });
    const b = await api("POST", "/issues", { project: KEY, title: "Cache the route tiles", description: "Short." });
    const c = await api("POST", "/issues", { project: KEY, title: "Print the timetable" });
    await api("POST", `/issues/${a.key}/relations`, { kind: "blocks", issue: b.key });
    await api("POST", `/issues/${a.key}/relations`, { kind: "relates", issue: c.key });
    await api("POST", `/issues/${a.key}/comments`, { body: `Compare ${c.key} and check **this** works.` });
    await call("POST", `/attachments?target=${encodeURIComponent("issue:" + a.key)}`, undefined, {
      multipart: { file: { name: "dot.png", mimeType: "image/png", buffer: Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "base64") }, caption: "A dot" },
    }).catch(() => {});
    list = [a, b, c];
  }
  ids.issue = list[0].key;
  const pg = await api("POST", "/pages", { title: "Layout plan", category: "plan", project: KEY, body: "The plan.\n\n- one\n- two" }).catch(() => null);
  const pages = await api("GET", `/pages?project=${KEY}`);
  ids.page = pages[0]?.id;
  await api("POST", "/glossary", { term: "UI checks glossary term", definition: "A term the checks use.", project: KEY }).catch(() => {});
  const gl = await api("GET", `/glossary?project=${KEY}`);
  ids.glossary = (Array.isArray(gl) ? gl : gl.entries ?? gl.items)?.[0]?.id;
  await api("POST", "/design", { title: "Board screen", kind: "screen", body: "The board.", project: KEY }).catch(() => {});
  await api("POST", "/design", { title: "Travellers find a route in one tap", kind: "goal", project: KEY }).catch(() => {});
  const notes = await api("GET", `/design?project=${KEY}`);
  ids.design = (Array.isArray(notes) ? notes : notes.notes ?? notes.items)?.[0]?.id;
  await api("POST", "/views", { name: "Open work", query: "is:open", pinned: true }).catch(() => {});
  const views = await api("GET", "/views").catch(() => []);
  ids.view = views[0]?.id;
  const envs = await api("GET", `/projects/${KEY}/environments`).catch(() => []);
  ids.envName = envs[0]?.name;
  void pg;
  return ids;
}

function urlsFor(routes, ids) {
  const idFor = (r) =>
    /\/views\//.test(r) ? ids.view : /\/pages\//.test(r) ? ids.page : /\/glossary\//.test(r) ? ids.glossary : /\/design\//.test(r) ? ids.design : null;
  const value = (r, name) =>
    name === "key" ? (r.startsWith("/issue/") ? ids.issue : ids.key) : name === "id" ? idFor(r) : name === "name" ? ids.envName : null;
  const urls = [];
  for (const r of routes) {
    const params = r.match(/\$(\w+)/g) ?? [];
    let url = r;
    let ok = true;
    for (const p of params) {
      const name = p.slice(1);
      const v = value(r, name);
      if (v == null) { ok = false; break; }
      url = url.replace(p, encodeURIComponent(v));
    }
    if (!ok) { skipped.push(r); continue; }
    urls.push({ route: r, url });
    if (TAB_SOURCES[r]) for (const t of new Set(TAB_SOURCES[r]())) urls.push({ route: `${r}?tab=${t}`, url: `${url}?tab=${t}` });
  }
  urls.push({ route: "/p/$key/board?issue=:key", url: `/p/${ids.key}/board?issue=${ids.issue}` });
  urls.push({ route: "/p/$key/list?issue=:key", url: `/p/${ids.key}/list?issue=${ids.issue}` });
  urls.push({ route: "/search?q=:text", url: "/search?q=route" });
  return urls;
}

const phone = { name: "phone 390", scheme: "light", vp: { width: 390, height: 844 }, categories: ["overflow", "clipped", "leak", "duplicate", "overlap", "empty-section", "stuck-status", "dead-avatar"] };
const desktop = { name: "desktop 1440", scheme: "dark", vp: { width: 1440, height: 900 }, categories: ["overflow", "clipped", "leak", "duplicate", "overlap", "empty-section", "stuck-status", "dead-avatar"] };
const wide = { name: "wide 1920", scheme: "light", vp: { width: 1920, height: 1080 }, categories: ["wide"] };
const lookList = [...looks([desktop, phone], [1]), ...(process.env.E2E_LOOKS === "one" ? [] : [wide])];

const routes = routesFromSource();
let ids;
let urls;
{
  const admin = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const r = await login(admin, { username: "robin", password: "correct horse battery" });
  const made = await r.request.fetch(base + "/api/v1/admin/users", {
    method: "POST",
    headers: H,
    data: { ...USER, email: "uicheck@example.com", display_name: "Ui Check", is_admin: true },
  });
  if (made.status() >= 300 && made.status() !== 409) {
    const t = await made.text();
    if (!/exist|taken/i.test(t)) throw new Error(`creating ${USER.username}: ${made.status()} ${t.slice(0, 200)}`);
  }
  await admin.close();
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const p = await login(ctx);
  ids = await seed(p);
  urls = urlsFor(routes, ids);
  await ctx.close();
}
console.log(`${routes.length} routes from router.tsx -> ${urls.length} pages${skipped.length ? `; no seed for: ${skipped.join(", ")}` : ""}`);

for (const look of lookList) {
  const ctx = await browser.newContext({
    viewport: look.vp,
    colorScheme: look.scheme,
    hasTouch: look.vp.width < 600,
    isMobile: look.vp.width < 600,
  });
  const first = await login(ctx);
  const queue = [...urls];
  const t0 = Date.now();
  const worker = async (w) => {
    const page = w === 0 ? first : await ctx.newPage();
    page.setDefaultTimeout(15000);
    page.on("pageerror", (e) => errors.push(`${look.name} pageerror ${page.url()}: ${e.message}`));
    for (let u = queue.shift(); u; u = queue.shift()) {
      try {
        await page.goto(base + u.url, { waitUntil: "load" });
        await page.waitForLoadState("networkidle", { timeout: 8000 }).catch(() => {});
        await page
          .waitForFunction(() => !/\b(Saving|Loading)…/.test(document.body.innerText), undefined, { timeout: 3000 })
          .catch(() => {});
        await page
          .waitForFunction(() => ![...document.querySelectorAll(".animate-pulse")].some((e) => e.getClientRects().length), undefined, { timeout: 4000 })
          .catch(() => {});
        await page.waitForTimeout(300);
        record(u.route, look.name, await page.evaluate(inPage, { category: look.categories.filter((c) => c !== "overlap") }));
        if (look.categories.includes("overlap")) {
          // elementFromPoint only sees what is in the viewport, so this look uses a tall viewport.
          await page.setViewportSize({ width: look.vp.width, height: 2600 });
          await page.waitForTimeout(400);
          record(u.route, look.name, await page.evaluate(inPage, { category: ["overlap"] }));
          await page.setViewportSize(look.vp);
        }
      } catch (e) {
        errors.push(`${look.name} ${u.route}: ${String(e.message).slice(0, 160)}`);
      }
    }
  };
  await Promise.all(Array.from({ length: CONCURRENCY }, (_, i) => worker(i)));
  visited += urls.length;
  console.log(`${look.name}: ${urls.length} pages in ${Math.round((Date.now() - t0) / 1000)}s`);
  await ctx.close();
}
await browser.close();

const rows = [...findings.entries()].sort(([a], [b]) => (a < b ? -1 : 1));
fs.writeFileSync(
  path.join(out, "ui-checks.json"),
  JSON.stringify(rows.map(([key, f]) => ({ key, ...f })), null, 2),
);
const perCat = {};
for (const [, f] of rows) perCat[f.category] = (perCat[f.category] ?? 0) + 1;
console.log("findings per category:", JSON.stringify(perCat));
// A page that threw or never loaded wasn't checked: that fails, as does a run that checked nothing.
if (errors.length) console.log(`FAIL ${errors.length} page errors:\n  ${errors.slice(0, 10).join("\n  ")}`);
if (!visited) console.log("FAIL no pages checked");

const baseline = fs.existsSync(baselineFile) ? JSON.parse(fs.readFileSync(baselineFile, "utf8")) : {};
if (process.env.UI_CHECKS_WRITE) {
  // A run that missed pages would drop their entries from the baseline.
  if (errors.length || !visited) process.exit(1);
  const next = {};
  for (const [k] of rows) next[k] = baseline[k] ?? "";
  // Only a full run (E2E_LOOKS=all, the default) may drop baseline entries; a single look keeps the ones it did not see.
  if (!everyLook) for (const [k, v] of Object.entries(baseline)) if (!(k in next)) next[k] = v;
  fs.writeFileSync(baselineFile, JSON.stringify(next, null, 2) + "\n");
  console.log(`baseline written: ${rows.length} findings -> ${baselineFile}`);
  process.exit(0);
}
const fresh = rows.filter(([k]) => !(k in baseline));
const gone = Object.keys(baseline).filter((k) => !findings.has(k));
if (gone.length) console.log(`${gone.length} baseline entries no longer seen (remove them with UI_CHECKS_WRITE=1):\n  ${gone.join("\n  ")}`);
if (errors.length || !visited) process.exit(1);
if (fresh.length) {
  console.log(`FAIL ${fresh.length} new UI findings (fix them, or if intended add them with UI_CHECKS_WRITE=1):`);
  for (const [k, f] of fresh) console.log(`  ${k}\n      [${f.looks.join(", ")}] ${f.details.join(" | ")}`);
  process.exit(1);
}
console.log(`ok   no new UI findings (${rows.length} known)`);
