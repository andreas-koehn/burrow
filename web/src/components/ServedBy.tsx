import { Link } from "react-router-dom";
import { StatusBadge } from "@/components/StatusBadge";
import { RelativeTime } from "@/components/RelativeTime";
import type { ServedState } from "@/lib/status";

export interface ServedByProps {
  state: ServedState;
  /** Token name of the client that serves it now, or served it last; "" when the relay names none. */
  client: string;
  /** The session that serves it now; "" unless it is online. */
  sessionId: string;
  /** When a client last served it. */
  lastSeen: string | null | undefined;
}

/**
 * The status of something a client serves, and who serves it: the client by
 * name, linked, while it is online; once it is gone, who served it last and
 * since when. One line for a service and for the provider it backs.
 */
export function ServedBy({ state, client, sessionId, lastSeen }: ServedByProps) {
  return (
    // Always there, so a change is announced: the reader may be waiting for it with a terminal open.
    <p role="status">
      <StatusBadge state={state} />
      {state === "online" && (
        <>
          {" · "}Served by{" "}
          {sessionId
            ? <Link className="link-inline" to={`/clients/${encodeURIComponent(sessionId)}`}>{client || "a client"}</Link>
            : client || "a client"}
        </>
      )}
      {state === "offline" && (client || lastSeen) && (
        <>
          {/* Nothing after the name when the relay names no time. */}
          {" · "}last served{client ? ` by ${client}` : ""} <RelativeTime value={lastSeen} empty="" />
        </>
      )}
    </p>
  );
}
