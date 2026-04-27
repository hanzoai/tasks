import { AlertCircle } from 'lucide-react'
import { Alert, AlertTitle, AlertDescription } from './ui/alert'

export function ErrorState({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error)
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>Failed to load</AlertTitle>
      <AlertDescription>{msg}</AlertDescription>
    </Alert>
  )
}
