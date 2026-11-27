/** `a` → `b` as unified diff hunks with `context` lines around each change. */
export function unifiedDiff(a: string, b: string, context = 3): string {
  const A = a.split('\n')
  const B = b.split('\n')
  const n = A.length
  const m = B.length
  type Op = { k: ' ' | '-' | '+'; t: string; a: number; b: number }
  const ops: Op[] = []
  if (n * m > 4_000_000) {
    A.forEach((t, i) => ops.push({ k: '-', t, a: i, b: 0 }))
    B.forEach((t, j) => ops.push({ k: '+', t, a: n, b: j }))
  } else {
    const w = m + 1
    const dp = new Int32Array((n + 1) * w)
    for (let i = n - 1; i >= 0; i--)
      for (let j = m - 1; j >= 0; j--)
        dp[i * w + j] =
          A[i] === B[j]
            ? dp[(i + 1) * w + j + 1] + 1
            : Math.max(dp[(i + 1) * w + j], dp[i * w + j + 1])
    let i = 0
    let j = 0
    while (i < n || j < m) {
      if (i < n && j < m && A[i] === B[j]) ops.push({ k: ' ', t: A[i++], a: i - 1, b: j++ })
      else if (j >= m || (i < n && dp[(i + 1) * w + j] >= dp[i * w + j + 1]))
        ops.push({ k: '-', t: A[i++], a: i - 1, b: j })
      else ops.push({ k: '+', t: B[j++], a: i, b: j - 1 })
    }
  }
  const ranges: [number, number][] = []
  ops.forEach((o, x) => {
    if (o.k === ' ') return
    const lo = Math.max(0, x - context)
    const hi = Math.min(ops.length - 1, x + context)
    const last = ranges[ranges.length - 1]
    if (last && lo <= last[1] + 1) last[1] = Math.max(last[1], hi)
    else ranges.push([lo, hi])
  })
  const out: string[] = []
  for (const [lo, hi] of ranges) {
    const part = ops.slice(lo, hi + 1)
    const oldCount = part.filter((o) => o.k !== '+').length
    const newCount = part.filter((o) => o.k !== '-').length
    const first = part[0]
    out.push(
      `@@ -${first.a + (oldCount ? 1 : 0)},${oldCount} +${first.b + (newCount ? 1 : 0)},${newCount} @@`,
    )
    for (const o of part) out.push(o.k + o.t)
  }
  return out.join('\n')
}
