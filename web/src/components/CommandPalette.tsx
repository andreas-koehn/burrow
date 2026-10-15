import { useState, useCallback, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Dialog, Input } from "@/components/ds";
import { P } from "@/auth/permissions";
import { destinationsFor } from "@/lib/destinations";
import { apiFetch } from "@/lib/api";
import type { AiGatewayKey, AiModel, AiProvider, Service, ClientView } from "@/lib/contract";

export interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Whether the caller holds any of the permissions. */
  can: (needs?: readonly string[]) => boolean;
  hasAiGateway: boolean;
}

interface PaletteItem {
  key: string;
  label: string;
  group: string;
  path: string;
}

/** The rows of an answer; anything but a list (not loaded, failed, a relay without the route) is none. */
function rows<T>(data: unknown): T[] {
  return Array.isArray(data) ? (data as T[]) : [];
}

const has = (text: string | undefined, q: string) => (text ?? "").toLowerCase().includes(q);

/**
 * Command palette — query/focusIdx reset on every open because Layout passes
 * key={paletteKey} (incremented by openPalette()) so React remounts the whole
 * component fresh regardless of which open path was used (⌘K or Search button).
 */
export function CommandPalette({
  open,
  onOpenChange,
  can,
  hasAiGateway,
}: CommandPaletteProps) {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [focusIdx, setFocusIdx] = useState(0);

  // Dedup with Layout's ["services"] query — same key, same data.
  const servicesQuery = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
    enabled: open,
  });

  // Clients — need tunnels:read:any; tolerate 403/network errors → empty list.
  const clientsQuery = useQuery({
    queryKey: ["clients"],
    queryFn: () =>
      apiFetch<ClientView[]>("/clients").catch(() => [] as ClientView[]),
    retry: false,
    enabled: open && can([P.tunnelsReadAny]),
  });

  // Providers, models and gateway keys, under the keys and fetches of their own pages, so
  // an answer one of those already has is here at once. Nobody the AI Gateway is not shown
  // to is asked on behalf of; a lookup that fails adds no rows and takes nothing else away.
  const ai = open && hasAiGateway;
  // ...and each is asked only of someone who holds a permission of its route (an http service
  // being connected shows the gateway, not what the role may read).
  const aiRead = [P.aiReadOwn, P.aiReadAny, P.aiConfigureOwn, P.aiConfigureAny];
  const providersQuery = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
    enabled: ai && can([P.aiReadOwn, P.aiReadAny, P.aiProvidersManage]),
  });
  const modelsQuery = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
    enabled: ai && can(aiRead),
  });
  const keysQuery = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
    enabled: ai && can(aiRead),
  });

  const q = query.toLowerCase();

  // Build the item list: the navigation description's destinations + entity matches.
  // Wrapped in useMemo so the array reference is stable and doesn't cause
  // the useCallback below to re-create on every render.
  const allItems = useMemo<PaletteItem[]>(() => {
    const destinations = destinationsFor({ can, hasAiGateway });

    // Filter destinations by query. A page whose own name matches comes before the
    // pages that only sit in a workspace of that name ("Services" is both, and the
    // name an Overview has here carries its workspace).
    const byName = (d: { entryLabel: string }) => has(d.entryLabel, q);
    const filteredDestinations = q === ""
      ? destinations
      : [
          ...destinations.filter(byName),
          ...destinations.filter((d) => !byName(d) && (has(d.label, q) || has(d.group, q))),
        ];

    const entityItems: PaletteItem[] = [];
    if (q !== "") {
      const providers = rows<AiProvider>(providersQuery.data);
      const services = rows<Service>(servicesQuery.data);
      // A service reachable through the AI gateway only is left to its provider, as in the
      // services list — but only to a provider that is listed here. Without one (the providers
      // not read, or not asked for) the service keeps its own row and stays findable.
      const standsFor = new Map<string, Service>();
      for (const svc of services) {
        if (svc.gateway_only && providers.some((p) => p.service_id === svc.id)) standsFor.set(svc.id, svc);
      }

      // Before the services: a provider and the service behind it often share a name,
      // and the provider's page is the one that says how an app uses it. A provider that
      // stands for a service answers to that service's name as well.
      for (const p of providers) {
        if (has(p.name, q) || has(p.slug, q) || has(standsFor.get(p.service_id)?.name, q)) {
          entityItems.push({
            key: `prov-${p.slug}`,
            label: `${p.name} — provider`,
            group: "Results",
            path: `/gateway/providers/${encodeURIComponent(p.slug)}`,
          });
        }
      }

      for (const svc of services) {
        if (standsFor.has(svc.id)) continue;
        if (has(svc.name, q)) {
          entityItems.push({
            key: `svc-${svc.id}`,
            label: `${svc.name} — service`,
            group: "Results",
            path: `/services/${svc.id}`,
          });
        }
      }

      for (const cl of rows<ClientView>(clientsQuery.data)) {
        if (has(cl.token_name, q)) {
          entityItems.push({
            key: `cl-${cl.session_id}`,
            label: `${cl.token_name} — client`,
            group: "Results",
            path: `/clients/${cl.session_id}`,
          });
        }
      }

      for (const m of rows<AiModel>(modelsQuery.data)) {
        if (has(m.name, q)) {
          entityItems.push({
            key: `model-${m.name}`,
            label: `${m.name} — model`,
            group: "Results",
            // The Models page scrolls to the row of that name and marks it.
            path: `/gateway/models?focus=${encodeURIComponent(m.name)}`,
          });
        }
      }

      for (const k of rows<AiGatewayKey>(keysQuery.data)) {
        if (has(k.name, q)) {
          entityItems.push({
            key: `key-${k.id}`,
            label: `${k.name} — gateway key`,
            group: "Results",
            path: "/gateway/keys",
          });
        }
      }
    }

    // Merge: static destinations first, then entity results.
    return [
      ...filteredDestinations.map((d) => ({
        key: `dest-${d.path}`,
        label: d.label,
        group: d.group,
        path: d.path,
      })),
      ...entityItems,
    ];
  }, [q, servicesQuery.data, clientsQuery.data, providersQuery.data, modelsQuery.data, keysQuery.data, can, hasAiGateway]);

  const activate = useCallback(
    (path: string) => {
      navigate(path);
      onOpenChange(false);
    },
    [navigate, onOpenChange],
  );

  // Keyboard handler attached to the dialog body.
  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setFocusIdx((i) => Math.min(allItems.length - 1, i + 1));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setFocusIdx((i) => Math.max(0, i - 1));
      } else if (e.key === "Enter") {
        e.preventDefault();
        const item = allItems[focusIdx];
        if (item) activate(item.path);
      }
    },
    [allItems, focusIdx, activate],
  );

  return (
    <Dialog open={open} onOpenChange={onOpenChange} size="md" title="Jump to…">
      <div
        style={{ display: "flex", flexDirection: "column", gap: 8 }}
        onKeyDown={handleKeyDown}
      >
        <Input
          autoFocus
          role="searchbox"
          aria-label="Search"
          placeholder="Search…"
          value={query}
          onChange={(e) => { setQuery(e.target.value); setFocusIdx(0); }}
        />

        {allItems.length === 0 ? (
          <div
            style={{
              padding: "12px 16px",
              color: "var(--muted-foreground)",
              fontSize: "0.875rem",
              textAlign: "center",
            }}
          >
            No matches
          </div>
        ) : (
          <div
            className="menu"
            role="listbox"
            aria-label="Navigation destinations"
            style={{ maxHeight: 360, overflowY: "auto" }}
          >
            {allItems.map((item, i) => (
              <div
                key={item.key}
                role="option"
                aria-selected={i === focusIdx}
                className={`menu-item${i === focusIdx ? " is-focus" : ""}`}
                onMouseEnter={() => setFocusIdx(i)}
                onClick={() => activate(item.path)}
                style={{ cursor: "pointer" }}
              >
                <span style={{ flex: 1 }}>{item.label}</span>
                {item.group !== "Results" && (
                  <span className="shortcut" style={{ fontSize: "0.75rem", color: "var(--muted-foreground)" }}>
                    {item.group}
                  </span>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </Dialog>
  );
}
