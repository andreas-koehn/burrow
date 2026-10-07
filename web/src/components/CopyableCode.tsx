import { Copy } from "lucide-react";
import { copyText } from "@/lib/clipboard";

export interface CopyButtonProps {
  text: string;
  /** Accessible name: says what is copied. */
  label: string;
}

export function CopyButton({ text, label }: CopyButtonProps) {
  return (
    <button type="button" className="icon-btn" aria-label={label} onClick={() => void copyText(text)}>
      <Copy size={13} aria-hidden="true" />
    </button>
  );
}

/** The block's own name, from the button's: "Copy settings for X" → "Settings for X". */
function boxName(label: string): string {
  const what = label.replace(/^Copy /, "");
  return what.charAt(0).toUpperCase() + what.slice(1);
}

export interface CopyableCodeProps extends CopyButtonProps {
  /** Wrap long lines instead of scrolling inside the block. */
  wrap?: boolean;
}

/** A block of text to paste somewhere else, next to the button that copies it. */
export function CopyableCode({ text, label, wrap }: CopyableCodeProps) {
  return (
    <div className="row gap-2 copyable-code">
      {wrap ? (
        <pre className="cmd-block wrap fill-rest"><code>{text}</code></pre>
      ) : (
        // Long lines scroll inside the block; focusable so the keyboard can scroll it.
        // A group and not a region, as in InstallLines: more landmarks would only be in the way.
        <pre className="cmd-block fill-rest" tabIndex={0} role="group" aria-label={boxName(label)}><code>{text}</code></pre>
      )}
      <CopyButton text={text} label={label} />
    </div>
  );
}
