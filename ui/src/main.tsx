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
import { fetcher } from './lib/api'

const root = document.getElementById('root')
if (!root) throw new Error('root element missing')

createRoot(root).render(
  <StrictMode>
    <SWRConfig value={{ fetcher, revalidateOnFocus: false, dedupingInterval: 2000 }}>
      <BrowserRouter basename="/_/tasks">
        <Routes>
          <Route path="/" element={<App />}>
            <Route index element={<Navigate to="/namespaces" replace />} />
            <Route path="namespaces" element={<NamespacesPage />} />
            <Route path="namespaces/:ns" element={<NamespaceDetailPage />} />
            <Route path="namespaces/:ns/workflows" element={<WorkflowsPage />} />
            <Route path="namespaces/:ns/workflows/:workflowId" element={<WorkflowDetailPage />} />
            <Route path="namespaces/:ns/schedules" element={<SchedulesPage />} />
            <Route path="namespaces/:ns/batches" element={<BatchesPage />} />
            <Route path="namespaces/:ns/deployments" element={<DeploymentsPage />} />
            <Route path="namespaces/:ns/nexus" element={<NexusPage />} />
            <Route path="support" element={<SupportPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </SWRConfig>
  </StrictMode>,
)
