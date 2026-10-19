import { useEffect, useRef } from "react";
import type { ReactNode } from "react";
import { BurrowMark } from "@/pages/Login";

/** The bare page of an approval (burrow login, an app asking for access): outside the dashboard shell. */
export function Shell({ children }: { children: ReactNode }) {
  return (
    <div className="signin-page">
      <main className="signin-container">
        <div className="signin-brand">
          <BurrowMark size={26} />
          <span>Burrow</span>
        </div>
        {children}
      </main>
    </div>
  );
}

/** What happened, said once and given the focus so that it is read out. */
export function Result({ kind, children }: { kind: "status" | "alert"; children: ReactNode }) {
  const ref = useRef<HTMLParagraphElement>(null);
  useEffect(() => { ref.current?.focus(); }, []);
  return (
    <p ref={ref} tabIndex={-1} role={kind} className={`notice-inline linkpage-result ${kind === "status" ? "ok" : "error"}`}>
      <span className="body">{children}</span>
    </p>
  );
}
