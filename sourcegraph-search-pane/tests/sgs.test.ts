import { expect, test } from 'claude-code/testing'

const PANE = { plugin: 'sgs', component: 'Pane', requestId: 'sgs' } as const
const PANE_PROPS = {
  title: 'Sourcegraph search',
  isFocused: false,
  bodyColumns: 80,
  placement: 'dock',
  scroll: { offset: 0, bodyRows: 30 },
  view: {},
} as const

const SRC_JSON = JSON.stringify({
  SourcegraphEndpoint: 'https://demo.sourcegraph.com',
  Results: [
    {
      __typename: 'FileMatch',
      file: { path: 'cmd/main.go', url: '/r/github.com/sourcegraph/sourcegraph/-/blob/cmd/main.go' },
      repository: { name: 'github.com/sourcegraph/sourcegraph' },
      lineMatches: [{ lineNumber: 9, preview: '  func main() {' }],
    },
  ],
  ResultCount: 1,
  LimitHit: false,
  ElapsedMilliseconds: 42,
  Alert: { Title: '', Description: '' },
})

test('/sgs runs src search and lists linked hits in the pane', async ($, on) => {
  const argvs: string[][] = []
  on('process.run', async (_$, e) => {
    argvs.push([...e.argv])
    return { value: { exitCode: 0, stdout: SRC_JSON, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('ui.open', async () => ({ value: { isPlaced: true } }))

  const ran = await $.command.run({ command: 'sgs', args: 'func main' })
  expect(argvs[0]).toEqual(['src', 'search', '-json', 'func main count:50'])
  expect(String(ran.text)).toMatch(/1 results in 42 ms/)

  const ui = await $.ui.mount({ ...PANE, props: PANE_PROPS, surface: 'terminal' })
  const link = await ui.find({ type: 'Link', label: /sourcegraph\/sourcegraph › cmd\/main\.go:10/ })
  expect(link).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /func main\(\) \{/ })).toBeDefined()
  await ui.unmount()
})

test('the model tool returns text results and keeps an explicit count', async ($, on) => {
  const argvs: string[][] = []
  on('process.run', async (_$, e) => {
    argvs.push([...e.argv])
    return { value: { exitCode: 0, stdout: SRC_JSON, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })

  const out = await $.tool.call({ tool: 'mcp__sgs__search', query: 'repo:sourcegraph count:5 main' })
  expect(argvs[0]?.[3]).toBe('repo:sourcegraph count:5 main')
  expect(String(out.result)).toMatch(/github\.com\/sourcegraph\/sourcegraph\/cmd\/main\.go:10/)
  expect(String(out.result)).toMatch(/Full results: https:\/\/demo\.sourcegraph\.com\/search\?q=/)
})

test('src failures surface as an error', async ($, on) => {
  on('process.run', async () => ({
    value: { exitCode: 1, stdout: '', stderr: 'error: 401 Unauthorized', isStdoutTruncated: false, isStderrTruncated: false },
  }))
  on('ui.open', async () => ({ value: { isPlaced: true } }))

  const ran = await $.command.run({ command: 'sgs', args: 'foo' })
  expect(String(ran.text)).toMatch(/401 Unauthorized/)
})
