// Experiments (WP-254): A/B tests of how agents work. The shapes the
// server's /experiments routes answer with; the factories are in queries.ts.

export interface ExperimentArm {
  key: string
  label: string
  control?: boolean
}

export interface ExperimentRun {
  id: number
  experiment: string
  weights: Record<string, number>
  params: Record<string, string>
  projects: string[]
  /** finished tasks per arm it stops at; 0 = none */
  target: number
  note?: string
  started_by?: string
  started_at: string
  ends_at?: string
  stopped_at?: string
  stop_reason?: string
  /** tasks per arm */
  assigned: Record<string, number>
  exposed: Record<string, number>
}

export interface Experiment {
  key: string
  title: string
  question: string
  arms: ExperimentArm[]
  weights: Record<string, number>
  params: { key: string; label: string; default: string }[]
  where: string
  exposure: string
  primary: string
  client?: boolean
  caveats: string[]
  live: ExperimentRun | null
  past: ExperimentRun[] | null
}

export interface Experiments {
  experiments: Experiment[]
  caveats: string[]
}

export interface ExperimentStartInput {
  weights?: Record<string, number>
  params?: Record<string, string>
  projects?: string[]
  days?: number
  target?: number
  note?: string
}

export interface ExperimentMetric {
  key: string
  label: string
  unit: 'usd' | 'tokens' | 'count' | 'hours'
}

export interface ExperimentStat {
  key: string
  n: number
  median: number | null
  mean: number | null
  ratio?: number
  low?: number
  high?: number
  p?: number
  p_adj?: number
}

export interface ExperimentArmResult extends ExperimentArm {
  weight: number
  assigned: number
  exposed: number
  measured: number
  done: number
  done_rate: number | null
  done_p?: number
  in_scope: number
  stats: ExperimentStat[]
}

export interface ExperimentResults {
  experiment: string
  title: string
  run: ExperimentRun
  scope: 'done' | 'all'
  primary: string
  metrics: ExperimentMetric[]
  arms: ExperimentArmResult[]
  srm: number | null
  exposed_srm: number | null
  mde?: number
  per_arm?: number
  verdict: string
  kind: 'few' | 'srm' | 'difference' | 'none'
  interim: boolean
  notes: string[] | null
  caveats: string[] | null
  from: string
  to: string
}
