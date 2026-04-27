import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Route, Routes, Navigate } from 'react-router-dom'
import { SWRConfig } from 'swr'
import './index.css'
import { App } from './App'
import { NamespacesPage } from './pages/Namespaces'
import { NamespaceDetailPage } from './pages/NamespaceDetail'
import { WorkflowsPage } from './pages/Workflows'
import { WorkflowDetailPage } from './pages/WorkflowDetail'
import { SchedulesPage } from './pages/Schedules'
import { BatchesPage } from './pages/Batches'
import { DeploymentsPage } from './pages/Deployments'
import { NexusPage } from './pages/Nexus'
import { SupportPage } from './pages/Support'
import { PageShell } from './components/PageShell'
import { fetcher } from './lib/api'

const root = document.getElementById('root')
if (!root) throw new Error('root element missing')

// Workflows owns its full-bleed layout (saved-views rail + empty hero).
// All other surfaces wrap in PageShell for consistent padding.

createRoot(root).render(
  <StrictMode>
    <SWRConfig value={{ fetcher, revalidateOnFocus: false, dedupingInterval: 2000 }}>
      <BrowserRouter basename="/_/tasks">
        <Routes>
          <Route path="/" element={<App />}>
            <Route index element={<Navigate to="/namespaces" replace />} />
            <Route path="namespaces" element={<PageShell><NamespacesPage /></PageShell>} />
            <Route path="namespaces/:ns" element={<PageShell><NamespaceDetailPage /></PageShell>} />
            <Route path="namespaces/:ns/workflows" element={<WorkflowsPage />} />
            <Route
              path="namespaces/:ns/workflows/:workflowId"
              element={<PageShell><WorkflowDetailPage /></PageShell>}
            />
            <Route path="namespaces/:ns/schedules" element={<PageShell><SchedulesPage /></PageShell>} />
            <Route path="namespaces/:ns/batches" element={<PageShell><BatchesPage /></PageShell>} />
            <Route
              path="namespaces/:ns/deployments"
              element={<PageShell><DeploymentsPage /></PageShell>}
            />
            <Route path="namespaces/:ns/nexus" element={<PageShell><NexusPage /></PageShell>} />
            <Route path="support" element={<PageShell><SupportPage /></PageShell>} />
          </Route>
        </Routes>
      </BrowserRouter>
    </SWRConfig>
  </StrictMode>,
)
