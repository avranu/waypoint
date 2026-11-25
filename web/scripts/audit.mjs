// `npm run audit`: npm audit at high and above, with a short list of waived
// advisories. npm audit has no ignore option, and an advisory with no patched
// release fails every branch until upstream ships one.
//
// A waiver only holds while all of these are true; otherwise the audit fails:
//   - the advisory is absent from the production-only audit (--omit=dev), so
//     it reaches nothing the built app ships;
//   - npm has no release of the package outside the advisory's range (once a
//     fix exists, bump or override to it and drop the waiver);
//   - the waiver's `until` date hasn't passed.
// Any other high or critical advisory fails as plain npm audit would.
//
// `--only ID,ID` (npm run audit -- --only ID,ID) judges just those advisories:
// the others are not reported. deploy/advisories.sh uses it to ask whether the
// advisories a change adds are waived, whatever else the tree has (WP-1660).
import { execFileSync } from 'node:child_process'

const waivers = [
  {
    id: 'GHSA-fc5q-39xc-vjpf',
    pkg: 'picomatch',
    why: 'ReDoS on crafted globs; reached only through vite, tinyglobby and fdir at build time, on globs from our own config. No patched picomatch exists yet (range <=4.0.7, 4.0.7 is the latest).',
    until: '2027-01-31',
  },
]

const severe = new Set(['high', 'critical'])

function npm(args) {
  try {
    return execFileSync('npm', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] })
  } catch (err) {
    if (typeof err.stdout === 'string' && err.stdout.trim() !== '') return err.stdout
    throw err
  }
}

function advisories(extra) {
  const report = JSON.parse(npm(['audit', '--json', ...extra]))
  if (report.error) throw new Error(`npm audit failed: ${JSON.stringify(report.error)}`)
  const found = new Map()
  for (const vuln of Object.values(report.vulnerabilities ?? {})) {
    for (const via of vuln.via) {
      if (typeof via !== 'object' || !severe.has(via.severity)) continue
      const id = via.url.split('/').pop()
      found.set(id, { id, pkg: via.name, range: via.range, title: via.title, url: via.url })
    }
  }
  return found
}

function fixPublished(pkg, range) {
  const latest = JSON.parse(npm(['view', pkg, 'dist-tags.latest', '--json']))
  const affected = [].concat(JSON.parse(npm(['view', `${pkg}@${range}`, 'version', '--json'])))
  return affected.includes(latest) ? null : latest
}

const onlyAt = process.argv.indexOf('--only')
if (onlyAt > 0 && !process.argv[onlyAt + 1]) {
  console.error('npm audit: --only needs advisory ids, comma-separated')
  process.exit(2)
}
const only = onlyAt > 0 ? new Set(process.argv[onlyAt + 1].split(',')) : null

const all = advisories([])
const prod = advisories(['--omit=dev'])
const today = new Date().toISOString().slice(0, 10)
const failures = []

for (const adv of all.values()) {
  if (only && !only.has(adv.id)) continue
  const waiver = waivers.find((w) => w.id === adv.id && w.pkg === adv.pkg)
  const label = `${adv.id} ${adv.pkg} ${adv.range}: ${adv.title} (${adv.url})`
  if (!waiver) {
    failures.push(label)
    continue
  }
  if (prod.has(adv.id)) {
    failures.push(`${label}\n    waived, but it reaches production dependencies`)
    continue
  }
  if (today > waiver.until) {
    failures.push(`${label}\n    waiver expired on ${waiver.until}; check for a fix or renew it`)
    continue
  }
  const fixed = fixPublished(adv.pkg, adv.range)
  if (fixed) {
    failures.push(`${label}\n    ${adv.pkg}@${fixed} is out of range: upgrade and drop the waiver`)
    continue
  }
  console.log(`waived ${label}\n    ${waiver.why}`)
}

if (failures.length > 0) {
  console.error(`npm audit: ${failures.length} high or critical advisories`)
  for (const f of failures) console.error(`  ${f}`)
  process.exit(1)
}
console.log('npm audit: no unwaived high or critical advisories')
