import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Copy } from "lucide-react";
import { Segmented } from "@/components/ds";
import { detectOs } from "@/lib/platform";
import { CLIENT_OS_OPTIONS, installLines, isHttpsOrigin, type ClientOs } from "@/lib/installSnippets";

export type InstallLineId = "install" | "login" | "run";

export interface InstallLinesProps {
  /** The address this dashboard is served from, e.g. window.location.origin. */
  relayOrigin: string;
  /** What `burrow http` publishes in the example; a port or host:port. */
  target?: string;
  /** Which lines to show. All three by default, and then they are numbered. */
  lines?: readonly InstallLineId[];
}

const ALL_LINES: readonly InstallLineId[] = ["install", "login", "run"];
const LINE_LABEL: Record<InstallLineId, string> = { install: "Install", login: "Sign in", run: "Run" };
// What the copy button of a line is called, and what is said once it worked.
const LINE_NAME: Record<InstallLineId, string> = { install: "install", login: "sign-in", run: "run" };

/**
 * The commands that bring a machine online: install the client from this
 * relay, sign it in, publish a port. No line ever holds a token; the sign-in
 * is approved in the browser, on /link.
 */
export function InstallLines({ relayOrigin, target, lines = ALL_LINES }: InstallLinesProps) {
  const [os, setOs] = useState<ClientOs>(() => detectOs());
  const [said, setSaid] = useState("");
  const clear = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(clear.current), []);

  const text = installLines(os, relayOrigin, target);
  const numbered = lines.length === ALL_LINES.length;

  async function copy(id: InstallLineId) {
    let message: string;
    try {
      // No clipboard on a page that is not served over https.
      if (!navigator.clipboard) throw new Error("no clipboard");
      await navigator.clipboard.writeText(text[id]);
      const name = LINE_NAME[id];
      message = `${name.charAt(0).toUpperCase()}${name.slice(1)} command copied.`;
    } catch {
      message = "Could not copy. Select the text instead.";
    }
    setSaid(message);
    clearTimeout(clear.current);
    clear.current = setTimeout(() => setSaid(""), 4000);
  }

  return (
    <div className="install-lines">
      {lines.includes("install") && (
        <Segmented aria-label="Operating system" options={CLIENT_OS_OPTIONS} value={os} onChange={setOs} />
      )}
      {!isHttpsOrigin(relayOrigin) && (
        <p className="muted small">
          This dashboard is not served over HTTPS. <code>burrow login</code> needs an https address,
          and the installer accepts plain http from localhost only.
        </p>
      )}
      {/* role stated outright: without list markers Safari drops the list semantics. */}
      <ol role="list" aria-label="Commands" className="install-lines-list">
        {lines.map((id, i) => (
          <li key={id} className="install-line">
            <span className="install-line-label">
              {numbered ? `${i + 1}. ` : ""}{LINE_LABEL[id]}
            </span>
            <div className="row gap-2">
              <pre className="cmd-block wrap fill-rest"><code>{text[id]}</code></pre>
              <button type="button" className="icon-btn" aria-label={`Copy ${LINE_NAME[id]} command`} onClick={() => void copy(id)}>
                <Copy size={13} aria-hidden="true" />
              </button>
            </div>
            {id === "login" && (
              <p className="muted small">
                It opens this dashboard in your browser: compare the code with the one in your terminal and
                approve. Or sign in with a token: add <code>--token -</code> and paste one from{" "}
                <Link className="link-inline" to="/clients?tab=tokens">Clients, tab Tokens</Link>.
              </p>
            )}
            {id === "run" && (
              <p className="muted small">
                Publishes what listens on that port of the machine; put your own port, or host:port, in its place.
              </p>
            )}
          </li>
        ))}
      </ol>
      {/* Always there, so that a screen reader hears the text when it arrives; the focus stays on the button. */}
      <p role="status" className="muted small install-lines-status">{said}</p>
    </div>
  );
}
