import { useEffect, useRef, useState } from 'react';

// usePolling fetches fn() immediately and every intervalMs, returning the last
// result or error. Keeps a mounted flag so late responses don't set state.
export function usePolling<T>(fn: () => Promise<T>, intervalMs = 5000) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;

    const run = async () => {
      try {
        const result = await fn();
        if (mounted.current) {
          setData(result);
          setError(null);
        }
      } catch (e) {
        if (mounted.current) setError(e instanceof Error ? e.message : String(e));
      }
    };

    run();
    const timer = setInterval(run, intervalMs);
    return () => {
      mounted.current = false;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return { data, error };
}
