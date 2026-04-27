// Empty / loading / error placeholders shared across every list page.
// Each is a tiny block of Tamagui primitives — no own primitive layer.

import { Card, H3, Paragraph, Spinner, XStack, YStack } from 'hanzogui'

export function Empty({
  title,
  hint,
  action,
}: {
  title: string
  hint?: string
  action?: React.ReactNode
}) {
  return (
    <Card p="$6" bg="$background" borderColor="$borderColor" borderWidth={1}>
      <YStack gap="$2" items="center">
        <H3 size="$6" color="$color">
          {title}
        </H3>
        {hint && (
          <Paragraph color="$placeholderColor" maxW={520} text="center">
            {hint}
          </Paragraph>
        )}
        {action ? <YStack mt="$3">{action}</YStack> : null}
      </YStack>
    </Card>
  )
}

export function ErrorState({ error }: { error: Error }) {
  return (
    <Card p="$5" bg="$background" borderColor="#7f1d1d" borderWidth={1}>
      <YStack gap="$2">
        <H3 size="$5" color="#fca5a5">
          Could not load
        </H3>
        <Paragraph
          color="$placeholderColor"
          fontFamily={'ui-monospace, SFMono-Regular, monospace' as never}
          fontSize="$2"
        >
          {error.message}
        </Paragraph>
      </YStack>
    </Card>
  )
}

export function LoadingState({ rows = 3 }: { rows?: number }) {
  return (
    <YStack gap="$3">
      <YStack height={28} bg="$borderColor" rounded="$2" opacity={0.5} width={200} />
      {Array.from({ length: rows }, (_, i) => (
        <YStack key={i} height={56} bg="$borderColor" rounded="$2" opacity={0.3} />
      ))}
    </YStack>
  )
}

export function Loading({ label }: { label?: string }) {
  return (
    <XStack gap="$2" items="center" p="$5" justify="center">
      <Spinner size="small" />
      {label ? (
        <Paragraph color="$placeholderColor" fontSize="$2">
          {label}
        </Paragraph>
      ) : null}
    </XStack>
  )
}
