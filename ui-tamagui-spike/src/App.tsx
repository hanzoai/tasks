// App shell — sidebar + top bar + outlet. Mirrors the structure of
// the deprecated React/shadcn UI but every primitive is Tamagui.
//
// The route tree lives in main.tsx so that <App> only owns layout.

import { Outlet, useParams } from 'react-router-dom'
import { XStack, YStack } from 'hanzogui'
import { Sidebar } from './components/Sidebar'
import { TopBar } from './components/TopBar'

export default function App() {
  const { ns } = useParams()
  return (
    <XStack height="100vh" bg="$background" items="stretch">
      <Sidebar ns={ns} />
      <YStack flex={1} minW={0}>
        <TopBar ns={ns} />
        <YStack flex={1} overflow="hidden">
          <YStack flex={1} overflow="scroll">
            <Outlet />
          </YStack>
        </YStack>
      </YStack>
    </XStack>
  )
}
