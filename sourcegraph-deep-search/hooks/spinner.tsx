import type { ClientModule } from 'claude-code'

const FRAMES = ['⣾', '⣽', '⣻', '⢿', '⡿', '⣟', '⣯', '⣷']
const INTERVAL_MS = 80

type Props = { label: string; color: string }

const Spinner: ClientModule<Props, number> = ({ label, color }, surface) => {
  const { Box, Text } = surface.elements
  if (surface.state === undefined) {
    surface.setState(0)
    surface.every(INTERVAL_MS, () => surface.setState(((surface.state ?? 0) + 1) % FRAMES.length))
  }
  return (
    <Box gap={1}>
      <Text color={color}>{FRAMES[surface.state ?? 0]}</Text>
      <Text dimColor>{label}</Text>
    </Box>
  )
}

export default Spinner
