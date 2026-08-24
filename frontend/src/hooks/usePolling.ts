import { useCallback, useEffect, useRef, useState } from 'react';

// usePolling fetches fn() immediately and every intervalMs, returning the last
// result or error. Keeps a mounted flag so late responses don't set state.
// Call refresh() after mutations so the UI reflects server-side changes
// immediately instead of waiting for the next poll tick.
export function usePolling<T>(fn: () => Promise<T>, intervalMs = 5000) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const mounted = useRef(true);
  const fnRef = useRef(fn);
  fnRef.current = fn;

  const run = useCallback(async () => {
    try {
      const result = await fnRef.current();
      if (mounted.current) {
        setData(result);
        setError(null);
      }
    } catch (e) {
      if (mounted.current) setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    run();
    const timer = setInterval(run, intervalMs);
    return () => {
      mounted.current = false;
      clearInterval(timer);
    };
  }, [run, intervalMs]);

  return { data, error, refresh: run };
}
