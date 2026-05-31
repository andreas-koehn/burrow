import { useState, useEffect, useCallback } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Dialog, Input } from "@/components/ds";
import { destinationsFor } from "@/lib/destinations";
import { apiFetch } from "@/lib/api";
import type { Service, ClientView } from "@/lib/contract";

export interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  isAdmin: boolean;
  hasAiEndpoints: boolean;
  firstHttpServiceId?: string;
}

export function CommandPalette({
  open,
  onOpenChange,
  isAdmin,
  hasAiEndpoints,
  firstHttpServiceId,
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

  // Clients — admin-gated; tolerate 403/network errors → empty list.
  const clientsQuery = useQuery({
    queryKey: ["clients"],
    queryFn: () =>
      apiFetch<ClientView[]>("/clients").catch(() => [] as ClientView[]),
    retry: false,
    enabled: open && isAdmin,
  });

  // Build the item list: static destinations + entity matches.
  const destinations = destinationsFor({ isAdmin, hasAiEndpoints, firstHttpServiceId });

  const q = query.toLowerCase();

  // Filter destinations by query.
  const filteredDestinations = destinations.filter(
    (d) =>
      q === "" ||
      d.label.toLowerCase().includes(q) ||
      d.group.toLowerCase().includes(q),
  );

  // Build entity items from services and clients.
  interface PaletteItem {
    key: string;
    label: string;
    group: string;
    path: string;
  }

  const entityItems: PaletteItem[] = [];
  if (q !== "") {
    const servicesList = Array.isArray(servicesQuery.data) ? servicesQuery.data : [];
    for (const svc of servicesList) {
      if (svc.name.toLowerCase().includes(q)) {
        entityItems.push({
          key: `svc-${svc.id}`,
          label: `${svc.name} — service`,
          group: "Results",
          path: `/services/${svc.id}`,
        });
      }
    }

    const clientsList = Array.isArray(clientsQuery.data) ? clientsQuery.data : [];
    for (const cl of clientsList) {
      if (cl.token_name.toLowerCase().includes(q)) {
        entityItems.push({
          key: `cl-${cl.session_id}`,
          label: `${cl.token_name} — client`,
          group: "Results",
          path: `/clients/${cl.session_id}`,
        });
      }
    }
  }

  // Merge: static destinations first, then entity results.
  const allItems: PaletteItem[] = [
    ...filteredDestinations.map((d) => ({
      key: `dest-${d.path}`,
      label: d.label,
      group: d.group,
      path: d.path,
    })),
    ...entityItems,
  ];

  // Reset focus when query changes.
  useEffect(() => {
    setFocusIdx(0);
  }, [query]);

  // Reset state on open/close.
  useEffect(() => {
    if (open) {
      setQuery("");
      setFocusIdx(0);
    }
  }, [open]);

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
    <Dialog open={open} onOpenChange={onOpenChange} title="Jump to…">
      <div
        style={{ display: "flex", flexDirection: "column", gap: 8, minWidth: 400 }}
        onKeyDown={handleKeyDown}
      >
        <Input
          autoFocus
          role="searchbox"
          aria-label="Search"
          placeholder="Search…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
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
