// Shared page padding wrapper. The Workflows page bypasses it because
// it wants edge-to-edge layout with its own header band.

import { YStack } from 'hanzogui'

export function PageShell({ children }: { children: React.ReactNode }) {
  return (
    <YStack flex={1} p="$6" gap="$5" maxW={1280} width="100%" self="center">
      {children}
    </YStack>
  )
}
