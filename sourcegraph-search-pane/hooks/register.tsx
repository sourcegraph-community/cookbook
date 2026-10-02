import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Hit, Search } from '../types'

const PANE = 'sgs'
const TITLE = 'Sourcegraph search'
const DEFAULT_COUNT = 50
const TOOL_MAX_HITS = 40

const current = atom({ plugin: 'sgs', key: 'current' } as const, {
  status: 'idle',
  query: '',
  endpoint: '',
  hits: [],
  total: 0,
  limitHit: false,
  elapsedMs: 0,
  seq: 0,
} as Search)

type LineMatch = { lineNumber?: number; preview?: string }
type RawResult = {
  __typename?: string
  file?: { path?: string; url?: string }
  repository?: { name?: string; url?: string }
  name?: string
  url?: string
  lineMatches?: LineMatch[]
}
type RawSearch = {
  SourcegraphEndpoint?: string
  Results?: RawResult[]
  ResultCount?: number
  LimitHit?: boolean
  ElapsedMilliseconds?: number
  Alert?: { Title?: string; Description?: string }
}

export function withCount(query: string) {
  return /\bcount:/.test(query) ? query : `${query} count:${DEFAULT_COUNT}`
}

export function searchUrl(endpoint: string, query: string) {
  return `${endpoint.replace(/\/$/, '')}/search?q=${encodeURIComponent(query)}&patternType=keyword`
}

export function parseHits(raw: RawSearch): Hit[] {
  const base = (raw.SourcegraphEndpoint ?? '').replace(/\/$/, '')
  const hits: Hit[] = []
  for (const r of raw.Results ?? []) {
    const repo = r.repository?.name ?? r.name ?? ''
    if (r.__typename === 'FileMatch' && r.file) {
      const path = r.file.path ?? ''
      const fileUrl = `${base}${r.file.url ?? ''}`
      const lines = r.lineMatches ?? []
      if (lines.length === 0) hits.push({ repo, path, line: 0, preview: '', url: fileUrl })
      for (const m of lines) {
        const line = (m.lineNumber ?? 0) + 1
        hits.push({ repo, path, line, preview: (m.preview ?? '').trim(), url: `${fileUrl}?L${line}` })
      }
    } else if (r.__typename === 'Repository') {
      hits.push({ repo, path: '', line: 0, preview: '', url: `${base}${r.url ?? ''}` })
    }
  }
  return hits
}

type Outcome = { ok: true; search: Search } | { ok: false; error: string; endpoint: string }

async function runSearch($: EngineInterface, query: string, seq: number): Promise<Outcome> {
  const ran = await $.process.run(['src', 'search', '-json', withCount(query)], { timeoutMs: 60_000 })
  if (ran.exitCode !== 0) {
    const error = (ran.stderr || ran.stdout).trim().split('\n').slice(-3).join(' ') || `src exited ${ran.exitCode}`
    return { ok: false, error, endpoint: '' }
  }
  let raw: RawSearch
  try {
    raw = JSON.parse(ran.stdout) as RawSearch
  } catch {
    return { ok: false, error: 'Could not parse src output as JSON.', endpoint: '' }
  }
  const alert = raw.Alert?.Title ? `${raw.Alert.Title}: ${raw.Alert.Description ?? ''}`.trim() : undefined
  const hits = parseHits(raw)
  return {
    ok: true,
    search: {
      status: hits.length === 0 && alert ? 'error' : 'done',
      query,
      endpoint: (raw.SourcegraphEndpoint ?? '').replace(/\/$/, ''),
      hits,
      total: raw.ResultCount ?? hits.length,
      limitHit: raw.LimitHit === true,
      elapsedMs: raw.ElapsedMilliseconds ?? 0,
      error: alert,
      seq,
    },
  }
}

async function searchIntoPane($: EngineInterface, query: string) {
  const { seq } = await update($, current, (now): Search => ({ ...now, status: 'working', query, error: undefined, seq: now.seq + 1 }))
  const outcome = await runSearch($, query, seq)
  await update($, current, (now): Search => {
    if (now.seq !== seq) return now
    return outcome.ok ? outcome.search : { ...now, status: 'error', error: outcome.error, hits: [], total: 0 }
  })
  return outcome
}

function asText(s: Search, max: number) {
  const lines = [`${s.total}${s.limitHit ? '+' : ''} results for \`${s.query}\` on ${s.endpoint} (${s.elapsedMs} ms)`]
  if (s.error) lines.push(`Alert: ${s.error}`)
  for (const hit of s.hits.slice(0, max)) {
    lines.push(hit.path ? `${hit.repo}/${hit.path}${hit.line ? `:${hit.line}` : ''}${hit.preview ? `  ${hit.preview.slice(0, 160)}` : ''}` : hit.repo)
  }
  if (s.hits.length > max) lines.push(`…${s.hits.length - max} more matches not shown`)
  lines.push(`Full results: ${searchUrl(s.endpoint, s.query)}`)
  return lines.join('\n')
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'sgs',
      description: 'Search code on Sourcegraph and show results in a side pane',
      argumentHint: '<query>',
    })
    await $.tool.register({
      name: 'search',
      description: [
        'Search code across every repository indexed by the user\'s Sourcegraph instance (the src CLI endpoint).',
        'Takes a Sourcegraph query: keywords plus filters like repo:, file:, lang:, type:symbol, count:, case:yes.',
        'Returns repo/path:line matches with a preview line and a link to the full results.',
        'Use it to find real code across repos without cloning them, e.g. to check that a code sample in docs matches the source.',
      ].join(' '),
      inputSchema: {
        type: 'object',
        properties: { query: { type: 'string', description: 'Sourcegraph search query' } },
        required: ['query'],
      },
    })
    return next(e)
  })

  on('command.run', { command: 'sgs' }, async ($, e) => {
    const query = e.args.trim()
    await $.ui.open({ id: PANE, title: TITLE })
    if (!query) return { text: 'Usage: /sgs <query>, e.g. /sgs repo:sourcegraph/sourcegraph lang:go func main' }
    const outcome = await searchIntoPane($, query)
    if (!outcome.ok) return { text: `sgs: ${outcome.error}` }
    const s = outcome.search
    return { text: `${s.total}${s.limitHit ? '+' : ''} results in ${s.elapsedMs} ms. ${searchUrl(s.endpoint, s.query)}` }
  })

  on('tool.call', { tool: 'mcp__sgs__search' }, async ($, e) => {
    const query = String((e as { query?: unknown }).query ?? '').trim()
    if (!query) return { result: 'Error: query is required.' }
    const outcome = await searchIntoPane($, query)
    return { result: outcome.ok ? asText(outcome.search, TOOL_MAX_HITS) : `Error: ${outcome.error}` }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Link } = $.ui.resolve(e)
    const s = await read($, current)
    const width = Math.max(30, e.props?.bodyColumns ?? 60)
    const room = Math.max(3, Math.floor(((e.viewport?.rows ?? 24) - 6) / 2))

    if (s.status === 'idle') return <Text dimColor>Run /sgs &lt;query&gt; to search code on Sourcegraph.</Text>

    return (
      <Box flexDirection="column">
        <Text bold>{s.query}</Text>
        {s.status === 'working' && <Text dimColor>Searching…</Text>}
        {s.status === 'error' && <Text color="red">{s.error}</Text>}
        {s.status === 'done' && (
          <Text dimColor>
            {s.total}
            {s.limitHit ? '+' : ''} results · {s.elapsedMs} ms
          </Text>
        )}
        {s.status !== 'working' &&
          s.hits.slice(0, room).map(hit => (
            <Box flexDirection="column" marginTop={1}>
              <Link href={hit.url} label={`${hit.repo.replace(/^github\.com\//, '')}${hit.path ? ` › ${hit.path}${hit.line ? `:${hit.line}` : ''}` : ''}`.slice(0, width)} />
              {hit.preview !== '' && <Text dimColor>{hit.preview.slice(0, width - 2)}</Text>}
            </Box>
          ))}
        {s.status === 'done' && s.endpoint !== '' && (
          <Box marginTop={1}>
            <Link href={searchUrl(s.endpoint, s.query)} label={s.hits.length > room ? `Open all ${s.total} results` : 'Open in Sourcegraph'} />
          </Box>
        )}
      </Box>
    )
  })
}
