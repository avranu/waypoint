// Prints the suites of one shard, one per line: node shard.mjs 2/5
//
// CI runs the suites as parallel jobs, each with a server of its own
// (run.sh --shard I/N). Suites are dealt out heaviest first to the shard with
// the least so far, by the seconds in weights.txt. A suite missing from it
// counts as the median one, so a new suite lands in a shard without anyone
// editing anything; refresh weights.txt from a run's out/timings.txt now and
// then (the balance drifts, nothing else breaks). 01-signup-flow creates the
// user the others sign in as and is in every shard (run.sh adds it), so it is
// not dealt out and every shard starts with its cost.
//
// .gitattributes marks weights.txt merge=union, so pull requests that each add
// a line merge without a conflict (git does; Waypoint's merge queue does it
// for the forge's own update, WP-594).
//
// A suite that needs another one's data (none do today) goes in the `together`
// list of weights.txt as `together a+b`: the group is dealt out as one.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const m = /^(\d+)\/(\d+)$/.exec(process.argv[2] ?? "");
if (!m || +m[1] < 1 || +m[1] > +m[2]) {
  console.error("usage: node shard.mjs I/N   (I from 1 to N)");
  process.exit(2);
}
const [index, count] = [+m[1] - 1, +m[2]];

const weight = new Map();
const groups = [];
for (const line of fs.readFileSync(path.join(here, "weights.txt"), "utf8").split("\n")) {
  const t = line.replace(/#.*/, "").trim();
  if (!t) continue;
  const g = /^together\s+(\S+)$/.exec(t);
  if (g) groups.push(g[1].split("+"));
  else {
    const [sec, name] = t.split(/\s+/);
    weight.set(name, Number(sec));
  }
}

const suites = fs
  .readdirSync(here)
  .filter((f) => /^\d.*\.mjs$/.test(f))
  .map((f) => f.replace(/\.mjs$/, ""))
  .filter((n) => !n.startsWith("01-"))
  .sort();
const known = [...weight.values()].sort((a, b) => a - b);
const median = known[Math.floor(known.length / 2)] ?? 30;
const seconds = (n) => weight.get(n) ?? median;

// Items to deal out: each group as one, every other suite alone.
const grouped = new Set(groups.flat());
const items = groups.map((g) => ({ names: g, sec: g.reduce((a, n) => a + seconds(n), 0) }));
for (const n of suites) if (!grouped.has(n)) items.push({ names: [n], sec: seconds(n) });
items.sort((a, b) => b.sec - a.sec || (a.names[0] < b.names[0] ? -1 : 1));

const load = Array(count).fill(0);
const mine = [];
for (const it of items) {
  const i = load.indexOf(Math.min(...load));
  load[i] += it.sec;
  if (i === index) mine.push(...it.names);
}
console.error(`shard ${index + 1}/${count}: ${mine.length} suites, about ${load[index]}s plus signup (${load.join(" ")})`);
for (const n of mine.sort()) console.log(n + ".mjs");
