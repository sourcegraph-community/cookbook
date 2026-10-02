export type Answer = {
  status: 'idle' | 'working' | 'done' | 'error'
  question: string
  markdown: string
  url?: string
  elapsedMs: number
  error?: string
  seq: number
}

declare module 'claude-code' {
  interface PluginState {
    'sourcegraph-deep-search': { current: Answer }
  }
}
