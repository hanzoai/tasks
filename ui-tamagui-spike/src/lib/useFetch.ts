// Tiny SWR replacement so the v2 spike has no extra runtime dep
// beyond hanzogui itself. Same semantics as the slice of SWR v1 used:
//   - keyed by URL string
//   - returns { data, error, isLoading, isValidating, mutate }
//   - revalidates on focus and on demand
//
// No caching across hook instances — each useFetch owns its data.
// That's enough for the Workflows page and keeps the bundle small.
// When we port more pages and need shared cache, swap this out for
// real SWR. Don't reinvent it.

import { useCallback, useEffect, useRef, useState } from 'react'
import { fetcher, ApiError } from './api'

export interface FetchState<T> {
  data: T | undefined
  error: ApiError | Error | undefined
  isLoading: boolean
  isValidating: boolean
  mutate: () => Promise<void>
}

export function useFetch<T>(url: string | null): FetchState<T> {
  const [data, setData] = useState<T | undefined>(undefined)
  const [error, setError] = useState<ApiError | Error | undefined>(undefined)
  const [isLoading, setIsLoading] = useState(url !== null)
  const [isValidating, setIsValidating] = useState(false)
  const generation = useRef(0)

  const run = useCallback(async () => {
    if (url === null) return
    const my = ++generation.current
    setIsValidating(true)
    try {
      const next = await fetcher<T>(url)
      if (my !== generation.current) return
      setData(next)
      setError(undefined)
    } catch (e) {
      if (my !== generation.current) return
      setError(e instanceof Error ? e : new Error(String(e)))
    } finally {
      if (my === generation.current) {
        setIsLoading(false)
        setIsValidating(false)
      }
    }
  }, [url])

  useEffect(() => {
    if (url === null) {
      setData(undefined)
      setError(undefined)
      setIsLoading(false)
      return
    }
    setIsLoading(true)
    run()
  }, [url, run])

  // Revalidate when the tab regains focus (matches SWR's default).
  useEffect(() => {
    const onFocus = () => {
      if (url !== null) run()
    }
    window.addEventListener('focus', onFocus)
    return () => window.removeEventListener('focus', onFocus)
  }, [run, url])

  return { data, error, isLoading, isValidating, mutate: run }
}
