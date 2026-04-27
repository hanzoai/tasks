// One-click copy field for connection strings (ZAP / HTTP endpoints).
// Mirrors the v1 ConnectDropdown copy block.

import { useState } from 'react'
import { Button, Text, XStack } from 'hanzogui'
import { Check, Copy } from '@hanzogui/lucide-icons-2'

export function CopyField({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  const onCopy = () => {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1200)
    })
  }
  return (
    <XStack
      items="center"
      gap="$1"
      px="$2"
      py="$1"
      rounded="$2"
      borderWidth={1}
      borderColor="$borderColor"
      bg={'rgba(255,255,255,0.02)' as never}
    >
      <Text
        flex={1}
        fontSize="$2"
        fontFamily={'ui-monospace, SFMono-Regular, monospace' as never}
        color="$color"
        numberOfLines={1}
      >
        {value}
      </Text>
      <Button size="$1" chromeless onPress={onCopy} aria-label="Copy">
        {copied ? <Check size={14} color="#86efac" /> : <Copy size={14} />}
      </Button>
    </XStack>
  )
}
