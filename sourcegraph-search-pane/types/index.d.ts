export type Hit = { repo: string; path: string; line: number; preview: string; url: string }
export type Search = {
  status: 'idle' | 'working' | 'done' | 'error'
  query: string
  endpoint: string
  hits: Hit[]
  total: number
  limitHit: boolean
  elapsedMs: number
  error?: string
  seq: number
}

declare module 'claude-code' {
  interface PluginState {
    sgs: { current: Search }
  }
}
