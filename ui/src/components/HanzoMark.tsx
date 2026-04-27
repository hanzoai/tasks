// Hanzo H mark — the same SVG path the deprecated React/shadcn UI
// painted. White on dark in the dark-default app shell.

import { XStack } from 'hanzogui'

export function HanzoMark() {
  return (
    <XStack
      width={28}
      height={28}
      rounded="$3"
      items="center"
      justify="center"
      bg={'#f2f2f2' as never}
    >
      <svg viewBox="0 0 24 24" width={16} height={16} fill="#070b13" aria-hidden="true">
        <path d="M4 3 H7 V10 H17 V3 H20 V21 H17 V13 H7 V21 H4 Z" />
      </svg>
    </XStack>
  )
}
