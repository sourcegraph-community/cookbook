import type { CommandRunInput } from 'claude-code'
import { expect, test } from 'claude-code/testing'

const PANE = { plugin: 'sourcegraph-deep-search', component: 'Pane', requestId: 'sourcegraph-deep-search' } as const
const PANE_PROPS = {
  title: 'Sourcegraph Deep Search',
  isFocused: false,
  bodyColumns: 80,
  placement: 'dock',
  scroll: { offset: 0, bodyRows: 30 },
  view: {},
} as const

const URL = 'https://demo.sourcegraph.com/deepsearch/abc123'
const ANSWER = JSON.stringify({
  text:
    'In the kubernetes/kubernetes (https://demo.sourcegraph.com/r/github.com/kubernetes/kubernetes) repo, 7 files still import io/ioutil:\n\n' +
    '| Repo | File | Status | Notes |\n|---|---|---|---|\n' +
    '| [github.com/kubernetes/kubernetes](https://demo.sourcegraph.com/r/github.com/kubernetes/kubernetes) | [staging/src/k8s.io/kubectl/pkg/cmd/helpers_test.go](https://demo.sourcegraph.com/r/github.com/kubernetes/kubernetes/-/blob/staging/src/k8s.io/kubectl/pkg/cmd/helpers_test.go?L20-L24) | still imports | test file |\n\n' +
    `Link: ${URL}`,
})

const CONFIGURED = { options: { sourcegraph_url: 'https://demo.sourcegraph.com' } } as const

const connected = { value: { isConnected: true, server: 'plugin:sourcegraph-deep-search:deepsearch' } } as const

test('/sourcegraph-deep-search asks Deep Search and shows the answer in the pane', CONFIGURED, async ($, on) => {
  const calls: { server: string; tool: string; args: Record<string, unknown> }[] = []
  on('mcp.connect', async () => connected)
  on('mcp.call', async (_$, e) => {
    calls.push({ server: e.server, tool: e.tool, args: e.args })
    return { value: { content: [{ type: 'text', text: ANSWER }], isError: false } }
  })
  on('ui.open', async () => ({ value: { isPlaced: true } }))

  const ran = await $.command.run({ command: 'sourcegraph-deep-search', args: 'where do kubernetes repos still use io/ioutil?' } as CommandRunInput)
  expect(calls).toEqual([{ server: 'plugin:sourcegraph-deep-search:deepsearch', tool: 'deepsearch', args: { question: 'where do kubernetes repos still use io/ioutil?' } }])
  expect(String(ran.text)).toContain(URL)

  const ui = await $.ui.mount({ ...PANE, props: PANE_PROPS, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: 'demo.sourcegraph.com' })).toBeDefined()
  expect(await ui.find({ type: 'Markdown', text: /^In the kubernetes\/kubernetes repo, 7 files/ })).toBeDefined()
  expect(await ui.find({ type: 'Markdown', text: /\]\(https:/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: 'kubernetes/kubernetes' })).toBeDefined()
  expect(await ui.find({ type: 'Link', text: 'staging/…/cmd/helpers_test.go:20-24' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'Status: still imports' })).toBeDefined()
  expect(await ui.find({ type: 'Markdown', text: 'test file' })).toBeDefined()
  expect(await ui.find({ type: 'Markdown', text: /\| File \|/ })).toBeUndefined()
  expect(await ui.find({ type: 'Markdown', text: /Link:/ })).toBeUndefined()
  expect(await ui.find({ type: 'Link', text: URL })).toBeDefined()
  await ui.unmount()
})

test('an unauthenticated server tells you to sign in', CONFIGURED, async ($, on) => {
  let called = 0
  on('mcp.connect', async () => ({ value: { isConnected: false, reason: 'auth', message: 'deepsearch needs sign-in.' } }))
  on('mcp.call', async () => {
    called++
    return { value: { content: [], isError: false } }
  })
  on('ui.open', async () => ({ value: { isPlaced: true } }))

  const ran = await $.command.run({ command: 'sourcegraph-deep-search', args: 'anything' } as CommandRunInput)
  expect(String(ran.text)).toMatch(/needs sign-in\. Run \/mcp/)
  expect(called).toBe(0)
})

test('a tool error surfaces as an error', CONFIGURED, async ($, on) => {
  on('mcp.connect', async () => connected)
  on('mcp.call', async () => ({ value: { content: [{ type: 'text', text: 'Deep Search is not enabled' }], isError: true } }))
  on('ui.open', async () => ({ value: { isPlaced: true } }))

  const ran = await $.command.run({ command: 'sourcegraph-deep-search', args: 'anything' } as CommandRunInput)
  expect(String(ran.text)).toMatch(/Deep Search is not enabled/)
})

test('no question prints usage', CONFIGURED, async ($, on) => {
  on('ui.open', async () => ({ value: { isPlaced: true } }))
  const ran = await $.command.run({ command: 'sourcegraph-deep-search', args: '  ' } as CommandRunInput)
  expect(String(ran.text)).toMatch(/^Usage: \/sourcegraph-deep-search <question>/)
})

test('no Sourcegraph URL asks you to set one', async ($, on) => {
  let connects = 0
  on('mcp.connect', async () => {
    connects++
    return connected
  })
  on('ui.open', async () => ({ value: { isPlaced: true } }))
  const ran = await $.command.run({ command: 'sourcegraph-deep-search', args: 'anything' } as CommandRunInput)
  expect(String(ran.text)).toMatch(/^Set your Sourcegraph URL first/)
  expect(connects).toBe(0)
  const ui = await $.ui.mount({ ...PANE, props: PANE_PROPS, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /^Set your Sourcegraph URL first/ })).toBeDefined()
  await ui.unmount()
})
