// Inline notice block used by detail pages (e.g. "stack trace requires
// running workflow", "synthetic timeline"). Two variants: default and
// destructive.

import { Card, H4, Paragraph, YStack } from 'hanzogui'

export function Alert({
  variant = 'default',
  title,
  children,
}: {
  variant?: 'default' | 'destructive'
  title: string
  children?: React.ReactNode
}) {
  const accent = variant === 'destructive' ? '#7f1d1d' : '$borderColor'
  const titleColor = variant === 'destructive' ? '#fca5a5' : '$color'
  return (
    <Card p="$4" bg="$background" borderColor={accent as never} borderWidth={1}>
      <YStack gap="$2">
        <H4 size="$4" color={titleColor as never}>
          {title}
        </H4>
        {children ? (
          <Paragraph color="$placeholderColor" fontSize="$2">
            {children}
          </Paragraph>
        ) : null}
      </YStack>
    </Card>
  )
}
