import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Route, Routes, Navigate } from 'react-router-dom'
import { SWRConfig } from 'swr'
import './index.css'
import { App } from './App'
import { NamespacesPage } from './pages/Namespaces'
import { WorkflowsPage } from './pages/Workflows'
import { WorkflowDetailPage } from './pages/WorkflowDetail'
import { SchedulesPage } from './pages/Schedules'
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
            <Route path="namespaces/:ns/workflows" element={<WorkflowsPage />} />
            <Route path="namespaces/:ns/workflows/:workflowId" element={<WorkflowDetailPage />} />
            <Route path="namespaces/:ns/schedules" element={<SchedulesPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </SWRConfig>
  </StrictMode>,
)
