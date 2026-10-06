import { Copy } from "lucide-react";
import { toast } from "sonner";

/** Copies to the clipboard and says so; the toast is shown by the toaster of the page. */
function copyText(text: string) {
  void navigator.clipboard?.writeText(text);
  toast.success("Copied.");
}

export interface CopyButtonProps {
  text: string;
  /** Accessible name: says what is copied. */
  label: string;
}

export function CopyButton({ text, label }: CopyButtonProps) {
  return (
    <button type="button" className="icon-btn" aria-label={label} onClick={() => copyText(text)}>
      <Copy size={13} aria-hidden="true" />
    </button>
  );
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
        // Long lines scroll inside the block; the keyboard has to reach it to scroll.
        <pre className="cmd-block fill-rest" tabIndex={0}><code>{text}</code></pre>
      )}
      <CopyButton text={text} label={label} />
    </div>
  );
}
