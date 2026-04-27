import { HanzoguiProvider } from 'hanzogui'
import config from '../hanzogui.config'
import { WorkflowsPage } from './pages/Workflows'

// v2 spike: hard-coded namespace=default. Production wiring (router,
// namespace switcher, IAM org → namespace mapping) lands when we port
// the chrome — not in this spike.
export default function App() {
  return (
    <HanzoguiProvider config={config} defaultTheme="dark">
      <WorkflowsPage namespace="default" />
    </HanzoguiProvider>
  )
}
