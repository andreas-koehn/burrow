import { toast } from "sonner";

/**
 * Copies to the clipboard and says whether that worked; the toast is shown by
 * the toaster of the page. The clipboard is missing on a page served without
 * TLS and a browser may refuse the write: then nothing was copied, and saying
 * "Copied." would leave the reader pasting something else.
 */
export async function copyText(text: string) {
  try {
    if (!navigator.clipboard) throw new Error("no clipboard");
    await navigator.clipboard.writeText(text);
    toast.success("Copied.");
  } catch {
    toast.error("Couldn't copy. Select the text and copy it yourself.");
  }
}
