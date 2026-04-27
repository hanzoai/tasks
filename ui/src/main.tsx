// Entry point. Wires HanzoguiProvider, BrowserRouter, and the route
// tree. Workflows renders edge-to-edge; every other page wraps in
// PageShell for consistent padding.

import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { HanzoguiProvider } from 'hanzogui'
import config from '../hanzogui.config'
import App from './App'
import { PageShell } from './components/PageShell'
import { NamespacesPage } from './pages/Namespaces'
import { NamespaceDetailPage } from './pages/NamespaceDetail'
import { WorkflowsPage } from './pages/Workflows'
import { WorkflowDetailPage } from './pages/WorkflowDetail'
import { WorkflowHistoryPage } from './pages/WorkflowHistory'
import { EventDetailPage } from './pages/EventDetail'
import { SchedulesPage } from './pages/Schedules'
import { BatchesPage } from './pages/Batches'
import { DeploymentsPage } from './pages/Deployments'
import { TaskQueuesPage } from './pages/TaskQueues'
import { TaskQueueDetailPage } from './pages/TaskQueueDetail'
import { WorkersPage } from './pages/Workers'
import { NexusPage } from './pages/Nexus'
import { SupportPage } from './pages/Support'

const root = document.getElementById('root')
if (!root) throw new Error('root element missing')

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <HanzoguiProvider config={config} defaultTheme="dark">
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
            <Route
              path="namespaces/:ns/workflows/:workflowId/history"
              element={<PageShell><WorkflowHistoryPage /></PageShell>}
            />
            <Route
              path="namespaces/:ns/workflows/:workflowId/events/:eventId"
              element={<PageShell><EventDetailPage /></PageShell>}
            />
            <Route path="namespaces/:ns/schedules" element={<PageShell><SchedulesPage /></PageShell>} />
            <Route path="namespaces/:ns/batches" element={<PageShell><BatchesPage /></PageShell>} />
            <Route
              path="namespaces/:ns/deployments"
              element={<PageShell><DeploymentsPage /></PageShell>}
            />
            <Route
              path="namespaces/:ns/task-queues"
              element={<PageShell><TaskQueuesPage /></PageShell>}
            />
            <Route
              path="namespaces/:ns/task-queues/:queue"
              element={<PageShell><TaskQueueDetailPage /></PageShell>}
            />
            <Route path="namespaces/:ns/workers" element={<PageShell><WorkersPage /></PageShell>} />
            <Route path="namespaces/:ns/nexus" element={<PageShell><NexusPage /></PageShell>} />
            <Route path="support" element={<PageShell><SupportPage /></PageShell>} />
          </Route>
        </Routes>
      </BrowserRouter>
    </HanzoguiProvider>
  </React.StrictMode>,
)
