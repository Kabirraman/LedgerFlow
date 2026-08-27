'use client';

/**
 * Data-fetching hooks.
 *
 * Hand-rolled rather than pulled from a library, for one reason: this console
 * polls. An operations view over live financial state has to keep moving without a
 * reload, and the polling contract — one in-flight request per key, aborted on
 * unmount, aborted when the key changes — is about sixty lines. A cache library
 * would be more capable and would also add a version whose revalidation semantics
 * this project does not verify.
 *
 * The `key` argument is the identity of a request. Change it and the hook refetches
 * and drops the previous response; pass null and it does not fetch at all, which is
 * how a screen waits for a route parameter or a role check.
 */

import { useCallback, useEffect, useRef, useState } from 'react';

import { ApiError } from './api';

export interface ApiState<T> {
  data: T | undefined;
  error: ApiError | undefined;
  /** True only on the first load for a given key. A poll in flight does not blank the screen. */
  loading: boolean;
  /** True while any request is in flight, including a background poll. */
  refreshing: boolean;
}

export interface UseApiResult<T> extends ApiState<T> {
  reload: () => void;
  /** Replaces the local copy without a round trip, for optimistic updates. */
  set: (next: T) => void;
}

interface UseApiOptions {
  /** Poll interval in milliseconds. Omit for a one-shot fetch. */
  pollMs?: number;
  /**
   * Suppresses polling while the tab is hidden. On by default: a console left open
   * on a second monitor for a day should not spend the day querying.
   */
  pauseWhenHidden?: boolean;
}

export function useApi<T>(
  key: string | null,
  fetcher: (signal: AbortSignal) => Promise<T>,
  options: UseApiOptions = {},
): UseApiResult<T> {
  const { pollMs, pauseWhenHidden = true } = options;

  const [state, setState] = useState<ApiState<T>>({
    data: undefined,
    error: undefined,
    loading: key !== null,
    refreshing: false,
  });
  const [nonce, setNonce] = useState(0);

  // The fetcher is almost always an inline closure, so it is a new function every
  // render. Holding it in a ref keeps it out of the effect's dependencies, which is
  // what stops every render from cancelling and reissuing the request.
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  const set = useCallback((next: T) => {
    setState((prev) => ({ ...prev, data: next, error: undefined }));
  }, []);

  useEffect(() => {
    if (key === null) {
      setState({ data: undefined, error: undefined, loading: false, refreshing: false });
      return;
    }

    let cancelled = false;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;

    const run = async (isFirst: boolean) => {
      if (cancelled) return;
      setState((prev) => ({
        ...prev,
        loading: isFirst,
        refreshing: true,
      }));
      try {
        const data = await fetcherRef.current(controller.signal);
        if (cancelled) return;
        setState({ data, error: undefined, loading: false, refreshing: false });
      } catch (err) {
        if (cancelled) return;
        if (err instanceof DOMException && err.name === 'AbortError') return;
        // A failed poll keeps the last good data on screen and shows the error
        // beside it. Blanking the view on a transient failure would make a brief
        // API blip look like "there are no cases".
        setState((prev) => ({
          ...prev,
          error: err instanceof ApiError ? err : new ApiError(0, 'error', String(err)),
          loading: false,
          refreshing: false,
        }));
      } finally {
        if (!cancelled && pollMs && pollMs > 0) {
          timer = setTimeout(() => void run(false), pollMs);
        }
      }
    };

    const shouldPause = () =>
      pauseWhenHidden && typeof document !== 'undefined' && document.visibilityState === 'hidden';

    const onVisibility = () => {
      if (!shouldPause() && !cancelled) {
        // Coming back to the tab should show current numbers immediately, not
        // whatever was true when the reader looked away.
        if (timer) clearTimeout(timer);
        void run(false);
      }
    };

    void run(true);
    if (pollMs && pollMs > 0 && typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', onVisibility);
    }

    return () => {
      cancelled = true;
      controller.abort();
      if (timer) clearTimeout(timer);
      if (typeof document !== 'undefined') {
        document.removeEventListener('visibilitychange', onVisibility);
      }
    };
  }, [key, nonce, pollMs, pauseWhenHidden]);

  return { ...state, reload, set };
}

export interface UseMutationResult<TArgs extends unknown[], TResult> {
  run: (...args: TArgs) => Promise<TResult | undefined>;
  pending: boolean;
  error: ApiError | undefined;
  result: TResult | undefined;
  reset: () => void;
}

/**
 * Wraps a one-shot write.
 *
 * `run` resolves to undefined on failure rather than rejecting, so a click handler
 * does not need a try/catch to avoid an unhandled rejection; the error is on the
 * returned state. Callers that need to branch check the resolved value.
 *
 * `pending` is what disables the button. That matters more here than in most apps:
 * a double-clicked Approve is a duplicate action request, and while the API's
 * idempotency key makes that safe, letting it happen at all wastes a round trip and
 * confuses the audit trail's timing.
 */
export function useMutation<TArgs extends unknown[], TResult>(
  fn: (...args: TArgs) => Promise<TResult>,
): UseMutationResult<TArgs, TResult> {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ApiError | undefined>(undefined);
  const [result, setResult] = useState<TResult | undefined>(undefined);

  const fnRef = useRef(fn);
  fnRef.current = fn;

  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const run = useCallback(async (...args: TArgs) => {
    setPending(true);
    setError(undefined);
    try {
      const value = await fnRef.current(...args);
      if (alive.current) {
        setResult(value);
        setPending(false);
      }
      return value;
    } catch (err) {
      if (alive.current) {
        setError(err instanceof ApiError ? err : new ApiError(0, 'error', String(err)));
        setPending(false);
      }
      return undefined;
    }
  }, []);

  const reset = useCallback(() => {
    setError(undefined);
    setResult(undefined);
  }, []);

  return { run, pending, error, result, reset };
}

/**
 * Delays a value, so typing in the case-queue search box does not issue one request
 * per keystroke.
 */
export function useDebounced<T>(value: T, ms = 300): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return debounced;
}

/**
 * A clock that ticks, for relative timestamps.
 *
 * Without this, "4m ago" stays "4m ago" until something else re-renders the row,
 * and an approval queue whose wait times are frozen is actively misleading to the
 * reviewer deciding what to pick up next.
 */
export function useNow(intervalMs = 30_000): number {
  const [now, setNow] = useState<number>(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

/**
 * False during server render and the first client render, true afterwards.
 *
 * Guards anything whose output depends on the reader's clock or timezone. Rendering
 * a locale-formatted date on the server and again in the browser produces two
 * different strings, and React resolves that mismatch by silently keeping one.
 */
export function useMounted(): boolean {
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);
  return mounted;
}
