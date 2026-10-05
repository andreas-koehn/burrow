import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";

/**
 * The query string as filter state: read the params, change one name with `setParam`
 * (an empty value removes it). The address is replaced, not pushed.
 *
 * The router applies a change a moment after it was asked for. Bound straight to its
 * params, a text box loses the caret, a checkbox flips back for a moment, and a second
 * change made in that moment drops the first. So the params returned here already hold
 * what was asked for, and the next change builds on them.
 */
export function useUrlParams(): [URLSearchParams, (name: string, value: string) => void] {
  const [params, setParams] = useSearchParams();
  const now = params.toString();
  // Query strings asked for that the address has not shown yet; the last one is current.
  const [pending, setPending] = useState<string[]>([]);
  const [seen, setSeen] = useState(now);
  if (now !== seen) {
    setSeen(now);
    // One of ours arriving: wait for the later ones. Anything else came from outside
    // (a link, the back button) and wins.
    const at = pending.lastIndexOf(now);
    setPending(at >= 0 ? pending.slice(at + 1) : []);
  } else if (pending.length > 0 && pending[pending.length - 1] === now) {
    // Changed and changed back before the address moved: nothing is outstanding.
    setPending([]);
  }
  const last = pending[pending.length - 1];
  const shown = useMemo(() => (last === undefined ? params : new URLSearchParams(last)), [last, params]);

  // What the next change builds on; moved on at once, so two changes in one tick add up.
  const base = useRef(shown);
  useEffect(() => { base.current = shown; }, [shown]);

  const setParam = useCallback((name: string, value: string) => {
    const next = new URLSearchParams(base.current);
    if (value) next.set(name, value); else next.delete(name);
    if (next.toString() === base.current.toString()) return;
    base.current = next;
    setPending((list) => [...list, next.toString()]);
    setParams(next, { replace: true });
  }, [setParams]);

  return [shown, setParam];
}
