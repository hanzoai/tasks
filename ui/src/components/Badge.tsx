// Status pill. Pure Tamagui — no shadcn. Color tokens come from
// lib/format.ts so the same palette is reused on every surface.

import { Text, XStack } from 'hanzogui'
import { badgeColors, type StatusVariant } from '../lib/format'

export function Badge({
  variant = 'muted',
  children,
}: {
  variant?: StatusVariant
  children: React.ReactNode
}) {
  const c = badgeColors(variant)
  return (
    <XStack
      px="$2"
      py="$1"
      rounded="$2"
      bg={c.bg as never}
      self="flex-start"
      items="center"
    >
      <Text fontSize="$1" color={c.fg as never} fontWeight="500">
        {children}
      </Text>
    </XStack>
  )
}
