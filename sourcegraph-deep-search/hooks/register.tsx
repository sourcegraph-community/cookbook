import { atom, read, update } from 'claude-code'
import type { EngineInterface, McpToolResult, Register } from 'claude-code'

import type { Answer } from '../types'

const PANE = 'sgs'
const TITLE = 'Sourcegraph Deep Search'
const SERVER = 'deepsearch'
const MAX_MARKDOWN = 10_000

const current = atom({ plugin: 'sourcegraph-deep-search', key: 'current' } as const, {
  status: 'idle',
  question: '',
  markdown: '',
  elapsedMs: 0,
  seq: 0,
} as Answer)

export function resultText(r: McpToolResult) {
  return r.content
    .map(b => (b.type === 'text' ? (b.text ?? '') : b.uri ?? ''))
    .filter(Boolean)
    .join('\n\n')
    .trim()
}

type Payload = { text?: unknown; [field: string]: unknown }

export function unwrap(raw: string): { markdown: string; fields: Payload } {
  if (!raw.startsWith('{')) return { markdown: raw, fields: {} }
  try {
    const parsed = JSON.parse(raw) as Payload
    return typeof parsed.text === 'string' ? { markdown: parsed.text.trim(), fields: parsed } : { markdown: raw, fields: {} }
  } catch {
    return { markdown: raw, fields: {} }
  }
}

export function linkify(markdown: string) {
  return markdown.replace(/(?<![\]\w])([^\s|()[\]]+) \((https?:\/\/[^\s)]+)\)/g, '[$1]($2)')
}

const SEPARATOR = /^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?$/

function cells(row: string) {
  return row.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map(c => c.trim())
}

export function tablesToLists(markdown: string) {
  const lines = markdown.split('\n')
  const out: string[] = []
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? ''
    if (!line.trim().startsWith('|') || !SEPARATOR.test((lines[i + 1] ?? '').trim())) {
      out.push(line)
      continue
    }
    i++
    while (i + 1 < lines.length && (lines[i + 1] ?? '').trim().startsWith('|')) {
      i++
      const row = cells(lines[i] ?? '').filter(Boolean)
      if (row.length) out.push(`- ${row.join(' · ')}`)
    }
  }
  return out.join('\n')
}

export function dropUrlLine(markdown: string, url: string | undefined) {
  if (!url) return markdown
  const lines = markdown.trimEnd().split('\n')
  const last = (lines[lines.length - 1] ?? '').trim()
  const bare = last.replace(/^[-*]?\s*(\*\*)?[\w ]{0,30}:(\*\*)?\s*/, '').replace(/^<|>$/g, '')
  return bare === url ? lines.slice(0, -1).join('\n').trimEnd() : markdown
}

const CONVERSATION = /https?:\/\/[^\s)\]>"']+\/deepsearch\/[^\s)\]>"']+/

export function conversationUrl(markdown: string, fields: Payload = {}) {
  for (const v of Object.values(fields)) {
    if (typeof v === 'string' && CONVERSATION.test(v)) return v.match(CONVERSATION)?.[0]
  }
  return markdown.match(CONVERSATION)?.[0]
}

export function clip(markdown: string) {
  return markdown.length <= MAX_MARKDOWN ? markdown : `${markdown.slice(0, MAX_MARKDOWN - 40).trimEnd()}\n\n…(truncated)`
}

type Outcome = { ok: true; answer: Answer } | { ok: false; error: string }

async function deepSearch($: EngineInterface, question: string, seq: number): Promise<Outcome> {
  const conn = await $.mcp.connect(SERVER)
  if (!conn.isConnected) {
    const hint = conn.reason === 'auth' ? ' Run /mcp and sign in to plugin:sourcegraph-deep-search:deepsearch.' : ''
    return { ok: false, error: `${conn.message}${hint}` }
  }
  const started = Date.now()
  let r: McpToolResult
  try {
    r = await $.mcp.call(conn.server, 'deepsearch', { question })
  } catch (err) {
    return { ok: false, error: `Deep Search call failed: ${err instanceof Error ? err.message : String(err)}` }
  }
  const { markdown, fields } = unwrap(resultText(r))
  if (r.isError) return { ok: false, error: markdown || 'Deep Search returned an error.' }
  if (!markdown) return { ok: false, error: 'Deep Search returned no answer.' }
  const url = conversationUrl(markdown, fields)
  return {
    ok: true,
    answer: { status: 'done', question, markdown: tablesToLists(linkify(dropUrlLine(markdown, url))), url, elapsedMs: Date.now() - started, seq },
  }
}

async function askIntoPane($: EngineInterface, question: string): Promise<Outcome> {
  const { seq } = await update($, current, (now): Answer => ({
    ...now,
    status: 'working',
    question,
    markdown: '',
    url: undefined,
    error: undefined,
    seq: now.seq + 1,
  }))
  const outcome = await deepSearch($, question, seq)
  await update($, current, (now): Answer => {
    if (now.seq !== seq) return now
    return outcome.ok ? outcome.answer : { ...now, status: 'error', error: outcome.error }
  })
  return outcome
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'sgs',
      description: 'Ask Sourcegraph Deep Search a question and show the answer in a side pane',
      argumentHint: '<question>',
    })
    return next(e)
  })

  on('command.run', { command: 'sgs' }, async ($, e) => {
    const question = e.args.trim()
    await $.ui.open({ id: PANE, title: TITLE })
    if (!question) return { text: 'Usage: /sgs <question>, e.g. /sgs where do Kubernetes repos still use the deprecated io/ioutil package?' }
    const outcome = await askIntoPane($, question)
    if (!outcome.ok) return { text: `sgs: ${outcome.error}` }
    const a = outcome.answer
    return { text: `sgs: answered in ${Math.round(a.elapsedMs / 1000)} s, see the pane.${a.url ? ` ${a.url}` : ''}` }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Link, Markdown } = $.ui.resolve(e)
    const a = await read($, current)

    if (a.status === 'idle') return <Text dimColor>Run /sgs &lt;question&gt; to ask Sourcegraph Deep Search.</Text>

    return (
      <Box flexDirection="column">
        <Text bold>{a.question}</Text>
        {a.status === 'working' && <Text dimColor>Deep Search is researching… this can take a minute or two.</Text>}
        {a.status === 'error' && <Text color="red">{a.error}</Text>}
        {a.status === 'done' && (
          <Box flexDirection="column" marginTop={1}>
            <Markdown text={clip(a.markdown)} />
            {a.url && (
              <Box marginTop={1}>
                <Link href={a.url} label="Open in Sourcegraph" />
              </Box>
            )}
          </Box>
        )}
      </Box>
    )
  })
}
