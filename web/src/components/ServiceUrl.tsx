import { Copy } from "lucide-react";
import { servicePath, serviceUrl } from "@/lib/serviceUrl";

export interface ServiceUrlProps {
  slug: string;
  url?: string;
}

/** A service's public URL: shows the path, copies the full URL. */
export function ServiceUrl({ slug, url }: ServiceUrlProps) {
  const full = serviceUrl(slug, url);
  if (!full) return <span className="muted">—</span>;
  return (
    <span className="row row-center gap-2">
      <span className="mono" title={full}>{servicePath(slug)}</span>
      <button
        type="button"
        className="icon-btn"
        aria-label={`Copy URL ${full}`}
        onClick={() => void navigator.clipboard?.writeText(full)}
      >
        <Copy size={13} />
      </button>
    </span>
  );
}
