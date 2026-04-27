import { Card } from './ui/card'

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
    <Card className="border-dashed py-10">
      <div className="text-center space-y-2">
        <p className="text-foreground">{title}</p>
        {hint && <p className="text-muted-foreground text-sm">{hint}</p>}
        {action && <div className="pt-2 flex justify-center">{action}</div>}
      </div>
    </Card>
  )
}
