// Top bar — namespace switcher (left), local-time toggle, theme
// toggle, account chip (right). Mirror of v1's TopBar with everything
// rendered through Tamagui primitives.

import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button, Popover, Text, XStack, YStack } from 'hanzogui'
import {
  ChevronDown,
  Clock,
  ExternalLink,
  LogOut,
  Moon,
  Sun,
  User,
} from '@hanzogui/lucide-icons-2'
import type { Namespace } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { getTz, setTz, type Tz } from '../lib/format'

export function TopBar({ ns }: { ns?: string }) {
  return (
    <XStack
      height={56}
      px="$5"
      gap="$3"
      borderBottomWidth={1}
      borderBottomColor="$borderColor"
      bg={'rgba(7,11,19,0.6)' as never}
      items="center"
    >
      <NamespaceDropdown activeNs={ns} />
      <XStack ml="auto" gap="$2" items="center">
        <LocalTimeIndicator />
        <ThemeToggle />
        <AccountChip />
      </XStack>
    </XStack>
  )
}

function NamespaceDropdown({ activeNs }: { activeNs?: string }) {
  const navigate = useNavigate()
  const { data } = useFetch<{ namespaces: Namespace[] }>('/v1/tasks/namespaces?pageSize=200')
  const all = data?.namespaces ?? []
  const [open, setOpen] = useState(false)

  return (
    <XStack items="center" gap="$1.5">
      <Popover open={open} onOpenChange={setOpen} placement="bottom-start">
        <Popover.Trigger asChild>
          <Button
            size="$2"
            chromeless
            borderWidth={1}
            borderColor="$borderColor"
            bg={'rgba(255,255,255,0.02)' as never}
          >
            <XStack items="center" gap="$1.5">
              <Text fontSize="$2" fontWeight="500" color="$color">
                {activeNs ?? 'default'}
              </Text>
              <ChevronDown size={14} color="#7e8794" />
            </XStack>
          </Button>
        </Popover.Trigger>
        <Popover.Content
          bg="$background"
          borderWidth={1}
          borderColor="$borderColor"
          p="$2"
          minW={220}
          elevate
        >
          <YStack gap="$1">
            <Text px="$2" py="$1" fontSize="$1" color="$placeholderColor" fontWeight="500">
              Switch namespace
            </Text>
            <YStack borderBottomWidth={1} borderBottomColor="$borderColor" my="$1" />
            {all.length === 0 ? (
              <Text px="$2" py="$1.5" fontSize="$2" color="$placeholderColor" opacity={0.6}>
                No namespaces
              </Text>
            ) : (
              all.map((nsItem) => {
                const name = nsItem.namespaceInfo.name
                const active = name === activeNs
                return (
                  <PopoverRow
                    key={name}
                    onPress={() => {
                      setOpen(false)
                      navigate(`/namespaces/${encodeURIComponent(name)}/workflows`)
                    }}
                  >
                    <Text fontSize="$2" fontWeight={active ? '600' : '400'} color="$color">
                      {name}
                    </Text>
                  </PopoverRow>
                )
              })
            )}
            <YStack borderBottomWidth={1} borderBottomColor="$borderColor" my="$1" />
            <PopoverRow
              onPress={() => {
                setOpen(false)
                navigate('/namespaces')
              }}
            >
              <Text fontSize="$2" color="$color">
                All namespaces…
              </Text>
            </PopoverRow>
          </YStack>
        </Popover.Content>
      </Popover>
      <a
        href="https://docs.hanzo.ai/tasks#namespaces"
        target="_blank"
        rel="noopener noreferrer"
        title="Learn about namespaces"
        style={{ display: 'inline-flex', padding: 4, color: '#7e8794' }}
      >
        <ExternalLink size={14} />
      </a>
    </XStack>
  )
}

function LocalTimeIndicator() {
  const [tz, setTzState] = useState<Tz>(getTz())
  const [now, setNow] = useState(() => new Date())
  const [open, setOpen] = useState(false)

  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 1000)
    return () => clearInterval(t)
  }, [])

  useEffect(() => {
    setTz(tz)
  }, [tz])

  const label =
    tz === 'utc'
      ? `UTC ${String(now.getUTCHours()).padStart(2, '0')}:${String(now.getUTCMinutes()).padStart(2, '0')}`
      : 'local'

  return (
    <Popover open={open} onOpenChange={setOpen} placement="bottom-end">
      <Popover.Trigger asChild>
        <Button
          size="$2"
          chromeless
          borderWidth={1}
          borderColor="$borderColor"
          bg={'rgba(255,255,255,0.02)' as never}
        >
          <XStack items="center" gap="$1.5">
            <Clock size={12} color="#7e8794" />
            <Text fontSize="$1" color="$placeholderColor">
              {label}
            </Text>
          </XStack>
        </Button>
      </Popover.Trigger>
      <Popover.Content
        bg="$background"
        borderWidth={1}
        borderColor="$borderColor"
        p="$2"
        minW={140}
        elevate
      >
        <YStack gap="$1">
          <Text px="$2" py="$1" fontSize="$1" color="$placeholderColor" fontWeight="500">
            Time zone
          </Text>
          <YStack borderBottomWidth={1} borderBottomColor="$borderColor" my="$1" />
          <PopoverRow
            onPress={() => {
              setTzState('local')
              setOpen(false)
            }}
          >
            <Text fontSize="$2" fontWeight={tz === 'local' ? '600' : '400'} color="$color">
              Local
            </Text>
          </PopoverRow>
          <PopoverRow
            onPress={() => {
              setTzState('utc')
              setOpen(false)
            }}
          >
            <Text fontSize="$2" fontWeight={tz === 'utc' ? '600' : '400'} color="$color">
              UTC
            </Text>
          </PopoverRow>
        </YStack>
      </Popover.Content>
    </Popover>
  )
}

function ThemeToggle() {
  // Tamagui owns the theme switch via HanzoguiProvider; we expose the
  // toggle UI but only flip the document class so global background
  // tracks. Persistent storage matches v1.
  const KEY = 'tasks.theme'
  const [theme, setTheme] = useState<'dark' | 'light'>(() => {
    if (typeof window === 'undefined') return 'dark'
    const stored = localStorage.getItem(KEY)
    if (stored === 'light' || stored === 'dark') return stored
    return 'dark'
  })
  useEffect(() => {
    if (typeof document === 'undefined') return
    document.documentElement.classList.toggle('dark', theme === 'dark')
    document.documentElement.classList.toggle('light', theme === 'light')
    localStorage.setItem(KEY, theme)
  }, [theme])
  return (
    <Button
      size="$2"
      chromeless
      onPress={() => setTheme((t) => (t === 'dark' ? 'light' : 'dark'))}
      aria-label="Toggle theme"
    >
      {theme === 'dark' ? <Sun size={16} color="#7e8794" /> : <Moon size={16} color="#7e8794" />}
    </Button>
  )
}

function AccountChip() {
  const [open, setOpen] = useState(false)
  return (
    <Popover open={open} onOpenChange={setOpen} placement="bottom-end">
      <Popover.Trigger asChild>
        <Button
          width={32}
          height={32}
          rounded={9999}
          borderWidth={1}
          borderColor="$borderColor"
          bg={'#f2f2f2' as never}
          aria-label="Account"
        >
          <Text fontSize="$1" fontWeight="600" color={'#070b13' as never}>
            HZ
          </Text>
        </Button>
      </Popover.Trigger>
      <Popover.Content
        bg="$background"
        borderWidth={1}
        borderColor="$borderColor"
        p="$2"
        minW={200}
        elevate
      >
        <YStack gap="$1">
          <YStack px="$2" py="$1.5">
            <Text fontSize="$2" fontWeight="500" color="$color">
              Hanzo
            </Text>
            <Text fontSize="$1" color="$placeholderColor">
              local · embedded
            </Text>
          </YStack>
          <YStack borderBottomWidth={1} borderBottomColor="$borderColor" my="$1" />
          <PopoverRow
            onPress={() => {
              setOpen(false)
              window.open('/v1/tasks/health', '_blank')
            }}
          >
            <XStack items="center" gap="$2">
              <User size={14} color="#7e8794" />
              <Text fontSize="$2" color="$color">
                Identity
              </Text>
            </XStack>
          </PopoverRow>
          <PopoverRow
            onPress={() => {
              setOpen(false)
              alert('Sign out is wired through hanzoai/gateway IAM session.')
            }}
          >
            <XStack items="center" gap="$2">
              <LogOut size={14} color="#7e8794" />
              <Text fontSize="$2" color="$color">
                Sign out
              </Text>
            </XStack>
          </PopoverRow>
        </YStack>
      </Popover.Content>
    </Popover>
  )
}

function PopoverRow({
  onPress,
  children,
}: {
  onPress: () => void
  children: React.ReactNode
}) {
  return (
    <XStack
      px="$2"
      py="$1.5"
      rounded="$2"
      hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
      cursor="pointer"
      onPress={onPress}
    >
      {children}
    </XStack>
  )
}
