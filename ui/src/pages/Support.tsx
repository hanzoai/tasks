import { ExternalLink, Github, BookOpen, MessageSquare } from 'lucide-react'
import { Card } from '../components/ui/card'

export function SupportPage() {
  return (
    <section className="space-y-4 max-w-3xl">
      <h2 className="text-lg font-medium">Support</h2>
      <div className="grid gap-3 sm:grid-cols-2">
        <SupportLink
          href="https://github.com/hanzoai/tasks"
          title="hanzoai/tasks on GitHub"
          hint="Source, issues, discussions"
          icon={Github}
        />
        <SupportLink
          href="https://hanzo.ai/docs/tasks"
          title="Documentation"
          hint="SDK guide, opcodes, IAM integration"
          icon={BookOpen}
        />
        <SupportLink
          href="https://github.com/hanzoai/tasks/issues/new"
          title="File an issue"
          hint="Bug reports, feature requests"
          icon={MessageSquare}
        />
        <SupportLink
          href="/healthz"
          title="Server health"
          hint="GET /healthz · GET /v1/tasks/health"
          icon={ExternalLink}
        />
      </div>
    </section>
  )
}

function SupportLink({
  href,
  title,
  hint,
  icon: Icon,
}: {
  href: string
  title: string
  hint: string
  icon: React.ComponentType<any>
}) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className="block"
    >
      <Card className="p-5 hover:bg-accent/40 transition-colors">
        <div className="flex items-start gap-3">
          <Icon size={18} className="text-muted-foreground mt-0.5" />
          <div className="flex-1 min-w-0">
            <p className="font-medium">{title}</p>
            <p className="text-sm text-muted-foreground">{hint}</p>
          </div>
          <ExternalLink size={14} className="text-muted-foreground" />
        </div>
      </Card>
    </a>
  )
}
