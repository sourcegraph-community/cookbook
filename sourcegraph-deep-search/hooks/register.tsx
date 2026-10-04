import { atom, read, update } from 'claude-code'
import type { EngineInterface, McpToolResult, Register } from 'claude-code'

import type { Answer } from '../types'

const PANE = 'sourcegraph-deep-search'
const TITLE = 'Sourcegraph Deep Search'
const SERVER = 'deepsearch'
const MAX_MARKDOWN = 10_000
const PAD_X = 2
const RULE = '─'.repeat(240)
const LOGO_PNG = 'assets/deep-search.png'
const LOGO_SVG = 'assets/deep-search.svg'
const ACCENT = '#F34E3F'
const WORKING = 'Deep Search is researching… this can take a minute or two.'

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
const MD_LINK = /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g
const ONE_LINK = /^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/
const FILE_HEADER = /file|path|location/i
const NOTES_HEADER = /note|detail|desc|why|reason|summary|explanation|comment/i
const EMPTY_CELL = /^(—|–|-|n\/a)?$/i

function cells(row: string) {
  return row.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map(c => c.trim())
}

export function elide(path: string, max = 48) {
  const parts = path.split('/')
  return path.length <= max || parts.length <= 3 ? path : `${parts[0]}/…/${parts.slice(-2).join('/')}`
}

export function lineRange(url: string) {
  const m = url.match(/[?&]L(\d+)(?:-L(\d+))?/)
  return m ? `:${m[1]}${m[2] ? `-${m[2]}` : ''}` : ''
}

export function linkLabel(label: string, url: string) {
  return url.includes('/-/blob/') ? `${elide(label)}${lineRange(url)}` : label.replace(/^github\.com\//, '')
}

export function unlink(markdown: string) {
  return markdown.replace(MD_LINK, (_, label: string, url: string) => linkLabel(label, url))
}

const plain = (cell: string) => unlink(cell).replace(/\*\*|`/g, '').trim()

export type Card = { title: string; file?: { label: string; href?: string }; meta: string[]; notes?: string }
export type Block = { kind: 'markdown'; text: string } | { kind: 'cards'; cards: Card[] }

function toCard(header: string[], row: string[]): Card {
  const fileCol = header.findIndex((h, i) => i > 0 && FILE_HEADER.test(h))
  const blobCol = row.findIndex((c, i) => i > 0 && c.includes('/-/blob/'))
  const file = fileCol > 0 ? fileCol : blobCol
  const named = header.findIndex((h, i) => i > 0 && i !== file && NOTES_HEADER.test(h))
  const notes = named > 0 ? named : row.length - 1 > 0 && row.length - 1 !== file ? row.length - 1 : -1
  const card: Card = { title: plain(row[0] ?? ''), meta: [] }
  row.forEach((cell, i) => {
    if (i === 0 || EMPTY_CELL.test(cell)) return
    if (i === file) {
      const one = cell.match(ONE_LINK)
      card.file = one ? { label: linkLabel(one[1] ?? '', one[2] ?? ''), href: one[2] } : { label: unlink(cell) }
    } else if (i === notes) card.notes = unlink(cell)
    else card.meta.push(`${plain(header[i] ?? '')}: ${plain(cell)}`)
  })
  return card
}

export function toBlocks(markdown: string): Block[] {
  const lines = markdown.split('\n')
  const blocks: Block[] = []
  let text: string[] = []
  const flush = () => {
    const t = unlink(text.join('\n')).trim()
    if (t) blocks.push({ kind: 'markdown', text: t })
    text = []
  }
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? ''
    if (!line.trim().startsWith('|') || !SEPARATOR.test((lines[i + 1] ?? '').trim())) {
      text.push(line)
      continue
    }
    flush()
    const header = cells(line)
    const cards: Card[] = []
    i++
    while (i + 1 < lines.length && (lines[i + 1] ?? '').trim().startsWith('|')) {
      i++
      const row = cells(lines[i] ?? '')
      if (row.some(Boolean)) cards.push(toCard(header, row))
    }
    if (cards.length) blocks.push({ kind: 'cards', cards })
  }
  flush()
  return blocks
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

export function instanceHost(url: unknown) {
  if (typeof url !== 'string' || !url.trim()) return undefined
  try {
    const u = new URL(url.trim())
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.host : undefined
  } catch {
    return undefined
  }
}

type Branding = { png?: string; svg?: string }
let branding: Promise<Branding> | undefined

function loadBranding($: EngineInterface): Promise<Branding> {
  const root = $.plugin.root
  const quiet = <T,>(p: Promise<T>) => p.catch(() => undefined)
  branding ??= Promise.all([
    quiet($.fs.read(`${root}/${LOGO_PNG}`, { as: 'bytes' })),
    quiet($.fs.read(`${root}/${LOGO_SVG}`)),
  ]).then(([png, svg]) => ({
    png: png?.base64,
    svg,
  }))
  return branding
}

export function stamp(ms: number) {
  return new Date(ms).toLocaleString('en-US', {
    month: '2-digit',
    day: '2-digit',
    year: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    hour12: true,
  })
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
    answer: { status: 'done', question, markdown: linkify(dropUrlLine(markdown, url)), url, elapsedMs: Date.now() - started, answeredAt: Date.now(), seq },
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

const SETUP = 'Set your Sourcegraph URL first: /config → sourcegraph-deep-search → Sourcegraph URL (e.g. https://sourcegraph.example.com), then run /reload-plugins.'

export const register: Register = (on, options) => {
  const host = instanceHost(options.sourcegraph_url)

  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'sourcegraph-deep-search',
      description: 'Ask Sourcegraph Deep Search a question and show the answer in a side pane',
      argumentHint: '<question>',
    })
    return next(e)
  })

  on('command.run', { command: 'sourcegraph-deep-search' }, async ($, e) => {
    const question = e.args.trim()
    await $.ui.open({ id: PANE, title: TITLE })
    if (!host) return { text: SETUP }
    if (!question) return { text: 'Usage: /sourcegraph-deep-search <question>, e.g. /sourcegraph-deep-search where do Kubernetes repos still use the deprecated io/ioutil package?' }
    const outcome = await askIntoPane($, question)
    if (!outcome.ok) return { text: `Deep Search: ${outcome.error}` }
    const a = outcome.answer
    return { text: `Deep Search: answered in ${Math.round(a.elapsedMs / 1000)} s, see the pane.${a.url ? ` ${a.url}` : ''}` }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const ui = $.ui.resolve(e)
    const { Box, Text, Link, Markdown } = ui
    const [a, brand] = await Promise.all([read($, current), loadBranding($)])

    const logo =
      'Image' in ui && brand.png ? (
        <ui.Image source={{ png: brand.png }} columns={2} rows={1} alt="✳" />
      ) : 'Svg' in ui && brand.svg ? (
        <ui.Svg source={brand.svg} width={20} height={20} alt="" />
      ) : null

    const header = (
      <Box justifyContent="space-between" marginBottom={1}>
        <Box gap={1}>
          {logo}
          <Text bold>Deep Search</Text>
        </Box>
        {host && <Text dimColor>{host}</Text>}
      </Box>
    )

    return (
      <Box flexDirection="column" paddingX={PAD_X} paddingY={1}>
        {header}
        {!host ? (
          <Text color={ACCENT}>{SETUP}</Text>
        ) : a.status === 'idle' ? (
          <Text dimColor>Run /sourcegraph-deep-search &lt;question&gt; to ask Sourcegraph Deep Search.</Text>
        ) : (
          <Box flexDirection="column">
            <Text bold>{a.question}</Text>
            {a.status === 'working' && (
              <Box marginTop={1}>
                {'Client' in ui ? (
                  <ui.Client key="spinner" module="./spinner.tsx" props={{ label: WORKING, color: ACCENT }} />
                ) : (
                  <Text dimColor>
                    <Text color={ACCENT}>✳</Text> {WORKING}
                  </Text>
                )}
              </Box>
            )}
            {a.status === 'error' && (
              <Box marginTop={1}>
                <Text color="red">{a.error}</Text>
              </Box>
            )}
            {a.status === 'done' && (
              <Box flexDirection="column" marginTop={1}>
                <Box flexDirection="column" gap={1}>
                  {toBlocks(clip(a.markdown)).map(b =>
                    b.kind === 'markdown' ? (
                      <Markdown text={b.text} />
                    ) : (
                      <Box flexDirection="column" gap={1}>
                        {b.cards.map(c => (
                          <Box flexDirection="column">
                            <Text bold color={ACCENT}>
                              {c.title}
                            </Text>
                            <Box flexDirection="column" paddingLeft={2}>
                              {c.file && (c.file.href ? <Link href={c.file.href} label={c.file.label} /> : <Markdown text={c.file.label} />)}
                              {c.meta.length > 0 && <Text dimColor>{c.meta.join(' · ')}</Text>}
                              {c.notes && <Markdown text={c.notes} />}
                            </Box>
                          </Box>
                        ))}
                      </Box>
                    ),
                  )}
                </Box>
                <Box marginTop={1} height={1} overflow="hidden">
                  <Text dimColor>{RULE}</Text>
                </Box>
                {a.url && (
                  <Text>
                    Open in Sourcegraph: <Link href={a.url} />
                  </Text>
                )}
                {a.answeredAt && (
                  <Box marginTop={1}>
                    <Text dimColor>{stamp(a.answeredAt)}</Text>
                  </Box>
                )}
              </Box>
            )}
          </Box>
        )}
      </Box>
    )
  })
}
