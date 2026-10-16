import { useId, useMemo, useState } from "react";
import { Link, Navigate } from "react-router-dom";
import { keepPreviousData, useQueries, useQuery, type Query } from "@tanstack/react-query";
import { ApiError, apiFetch } from "@/lib/api";
import { parseTimeRange, timeRangeMs, widerRange, type TimeRange } from "@/lib/time-range";
import { withAIConfigDefaults } from "@/lib/aiConfig";
import { useDebounced } from "@/lib/use-debounced";
import { useUrlParams } from "@/lib/use-url-params";
import { droppedNames, pairWords } from "@/lib/translation";
import { InspectorOffHint } from "@/components/InspectorOffHint";
import { Badge, Button, EmptyState, ErrorNotice, PageHeader, Select, type SelectOption } from "@/components/ds";
import { RelativeTime } from "@/components/RelativeTime";
import { Latency } from "@/components/Latency";
import { LogView, type LogColumn } from "@/components/LogView";
import type { AiCapturedRequest, AiGatewayKey, AiModel, AiProvider, InspectorEntry, Service, ServiceAIConfig } from "@/lib/contract";

/** How many requests the page asks for, newest first; the relay would give 200 unasked and at most 1000. */
const LIMIT = 100;
/** No event stream carries captured requests across providers; the list is read again this often. */
const POLL_MS = 5000;
/** Answers that asking again would only repeat: not signed in, no permission, no such list (an older relay). */
const FINAL = [401, 403, 404];
/**
 * Read again every few seconds, also after a failure that can pass (the network,
 * a relay that restarts); only a final answer ends it. The query client pauses
 * the interval while the tab is hidden.
 */
const poll = (q: Query<AiCapturedRequest[]>) => {
  const e = q.state.error;
  return e instanceof ApiError && FINAL.includes(e.status) ? false : POLL_MS;
};

const ALL: SelectOption = { value: "", label: "All" };

/** The list query: period, search and the pickers are applied by the relay, before its limit. */
function listQuery(range: TimeRange, search: string, pickers: Record<string, string> = {}): string {
  const p = new URLSearchParams();
  for (const [name, value] of Object.entries(pickers)) if (value) p.set(name, value);
  const ms = timeRangeMs(range);
  if (ms !== null) p.set("since", new Date(Date.now() - ms).toISOString());
  if (search) p.set("q", search);
  p.set("limit", String(LIMIT));
  return p.toString();
}

/** The options of a picker, with the value a link arrived with when the list does not know it. */
function withChosen(options: SelectOption[], value: string): SelectOption[] {
  return value && !options.some((o) => o.value === value) ? [...options, { value, label: value }] : options;
}

function Picker({ label, value, options, onChange }: {
  label: string; value: string; options: SelectOption[]; onChange: (value: string) => void;
}) {
  const id = useId();
  return (
    <span className="filter-label">
      <label htmlFor={id}>{label}</label>
      <Select id={id} options={options} value={value} onChange={onChange} />
    </span>
  );
}

/**
 * The requests the relay captured, across all providers, narrowed by provider, model
 * and gateway key. A row opens the request inspector, where a request is read and replayed.
 */
export default function Requests() {
  // Period, search and the pickers live in the URL, so a link reproduces the view.
  const [params, setParam] = useUrlParams();
  const range = parseTimeRange(params.get("range"));
  const search = params.get("q") ?? "";
  // The box and the URL follow every key; the relay is asked once typing pauses.
  const query = useDebounced(search, 250);
  const model = params.get("model") ?? "";
  const gatewayKey = params.get("gateway_key") ?? "";

  const setRange = (r: TimeRange) => setParam("range", r === "24h" ? "" : r);

  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });
  const providerList = useMemo(() => (Array.isArray(providers.data) ? providers.data : []), [providers.data]);
  const models = useQuery({
    queryKey: ["ai", "models"],
    queryFn: () => apiFetch<AiModel[]>("/ai/models"),
    retry: false,
  });
  const keys = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
  });

  // Links from before the pickers name a service: ?service=<id> means its provider.
  const linkedService = params.get("provider") ? "" : params.get("service") ?? "";
  const linkedProvider = linkedService ? providerList.find((p) => p.service_id === linkedService)?.slug ?? "" : "";
  const provider = params.get("provider") || linkedProvider;
  // A service that backs no provider keeps its requests in its own inspector; the
  // link is sent on there. An id that is no service at all narrows nothing.
  const noProvider = linkedService !== "" && Array.isArray(providers.data) && !linkedProvider;
  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
    enabled: noProvider,
  });
  const ownInspector = noProvider && Array.isArray(services.data) && services.data.some((s) => s.id === linkedService);
  // Until both are in, such a link does not say what it means.
  const resolving = linkedService !== "" && (providers.isLoading || (noProvider && services.isLoading));
  const setProvider = (slug: string) => {
    setParam("service", "");
    setParam("provider", slug);
  };

  // A relay from before the list across providers answers it with 404; its requests
  // are then read from one provider's service at a time.
  const [olderRelay, setOlderRelay] = useState(false);

  const across = useQuery({
    queryKey: ["inspector", "across", provider, model, gatewayKey, range, query],
    queryFn: () =>
      apiFetch<AiCapturedRequest[]>(`/ai/requests?${listQuery(range, query, { provider, model, gateway_key: gatewayKey })}`),
    retry: false,
    enabled: !olderRelay && !resolving,
    // A changed period or picker keeps the rows on screen until the new ones arrive;
    // every row names its provider and model, so none shows under a wrong heading.
    placeholderData: keepPreviousData,
    refetchInterval: poll,
  });
  if (!olderRelay && across.error instanceof ApiError && across.error.status === 404) setOlderRelay(true);

  // On an older relay one provider is always chosen: the one in the URL, else the first.
  const single = olderRelay ? providerList.find((p) => p.slug === (provider || providerList[0]?.slug)) : undefined;
  const ofOne = useQuery({
    // Under the inspector's own key, so everything that refreshes ["inspector", id] refreshes this too.
    queryKey: ["inspector", single?.service_id ?? "", "list", range, query],
    queryFn: () =>
      apiFetch<InspectorEntry[]>(`/services/${single!.service_id}/inspector/requests?${listQuery(range, query)}`)
        // Such an entry does not say who answered: it is the provider whose service holds it.
        .then((rows) => rows.map((r): AiCapturedRequest => ({
          ...r, provider: r.provider || single!.slug, model: r.model ?? "", gateway_key_id: r.gateway_key_id ?? "",
        }))),
    retry: false,
    enabled: Boolean(single),
    placeholderData: keepPreviousData,
    refetchInterval: poll,
  });

  const list = olderRelay ? ofOne : across;
  // A provider the older relay does not know has no requests; without the providers there is nothing to ask.
  const rows = olderRelay && !single ? (providers.data ? [] : undefined) : list.data;
  const failed = olderRelay && !single && providers.isError ? providers : list;
  const shownProvider = single?.slug ?? provider;

  // Why a list is empty: capture may be switched off. Asked only when there is nothing to show.
  const inScope = shownProvider ? providerList.filter((p) => p.slug === shownProvider) : providerList;
  const isEmpty = rows?.length === 0;
  const configs = useQueries({
    queries: inScope.map((p) => ({
      queryKey: ["service", p.service_id, "ai-config"],
      queryFn: () =>
        apiFetch<Partial<ServiceAIConfig>>(`/services/${p.service_id}/ai-config`).then(withAIConfigDefaults),
      retry: false,
      enabled: isEmpty,
    })),
  });
  // With all providers, capture is the reason only when it is off for every one of them.
  const captureOff = inScope.length > 0 && configs.every((c) => c.data && !c.data.inspector.enabled);

  const header = (
    <PageHeader
      title="Requests"
      subtitle="Requests the relay captured, across all providers. Open one to inspect and replay it."
    />
  );

  if (ownInspector) return <Navigate replace to={`/gateway/requests/${encodeURIComponent(linkedService)}`} />;

  // The page is always there; with nothing to list it says what is missing.
  // Known to be empty; a provider list that failed to load is not "no providers".
  if (Array.isArray(providers.data) && providers.data.length === 0 && !resolving) {
    return (
      <div className="inspector-page">
        {header}
        <EmptyState
          title="No providers yet"
          action={<Link className="btn btn-primary btn-sm" to="/gateway/providers">Add a provider</Link>}
        >
          Requests appear here once a provider is set up, receives traffic and has its request inspector switched on.
        </EmptyState>
      </div>
    );
  }

  const providerName = new Map(providerList.map((p) => [p.slug, p.name || p.slug]));

  // Tokens and cost are left out: the relay records neither on a captured request.
  const columns: LogColumn<AiCapturedRequest>[] = [
    { id: "time", header: "When", cell: (r) => <RelativeTime value={r.ts} /> },
    // A provider renamed since keeps its old slug here.
    { id: "provider", header: "Provider", cell: (r) => (r.provider ? providerName.get(r.provider) ?? r.provider : "—") },
    // Only a request made with a gateway key names a model.
    { id: "model", header: "Model", cell: (r) => r.model || "—" },
    { id: "path", header: "Path", cell: (r) => <Link className="mono" to={`/gateway/requests/${r.service_id}/${r.id}`}>{r.path}</Link> },
    { id: "status", header: "Status", cell: (r) => <Badge nodot kind={`status-${Math.floor(r.status / 100)}xx`}>{r.status}</Badge> },
    { id: "latency", header: "Latency", cell: (r) => <Latency ms={r.duration_ms} /> },
    { id: "cache", header: "Cache", cell: (r) => <span className="mono">{r.cache}</span> },
    {
      id: "guardrail",
      header: "Guardrail",
      cell: (r) => {
        const n = (r.redactions ?? []).reduce((sum, x) => sum + x.count, 0);
        return n > 0 ? `${n} redacted` : "—";
      },
    },
    {
      id: "translation",
      header: "Translation",
      cell: (r) => {
        if (!r.translated) return "—";
        const { names, more } = droppedNames(r.dropped);
        return (
          <span className="format-mode">
            <Badge nodot kind="status-idle">translated</Badge>
            {/* The direction is in the request's detail; here it is for a screen reader. */}
            <span className="visually-hidden">{pairWords(r.translated)}</span>
            {names.length > 0 && <span className="muted small">{`${names.length}${more ? "+" : ""} left out`}</span>}
          </span>
        );
      },
    },
  ];

  // What the pickers and the Filter box narrow; the older relay knows neither model nor gateway key.
  const filtered = Boolean(provider || query || (!olderRelay && (model || gatewayKey)));
  const clearFilters = () => {
    for (const name of ["service", "provider", "model", "gateway_key", "q"]) setParam(name, "");
  };

  const wider = widerRange(range);
  const showWider = wider && <Button variant={filtered ? "secondary" : "primary"} size="sm" onClick={() => setRange(wider.value)}>{wider.label}</Button>;
  let empty;
  if (captureOff && shownProvider) {
    empty = (
      <EmptyState title={`Request inspector is off for ${providerName.get(shownProvider) ?? shownProvider}`}>
        <InspectorOffHint serviceId={inScope[0]!.service_id} />
      </EmptyState>
    );
  } else if (captureOff) {
    empty = (
      <EmptyState title="Request inspector is off for every provider">
        Choose a provider to see how to turn it on.
      </EmptyState>
    );
  } else if (filtered) {
    empty = (
      <EmptyState
        title="No requests match these filters."
        // Inline, so the card centres them; the blank keeps the two apart.
        action={<><Button variant="primary" size="sm" onClick={clearFilters}>Clear filters</Button>{" "}{showWider}</>}
      />
    );
  } else if (wider) {
    empty = <EmptyState title="No requests in this period" action={showWider} />;
  } else {
    empty = (
      <EmptyState title="No requests yet">
        Requests appear here as soon as traffic reaches a provider whose request inspector is on.
      </EmptyState>
    );
  }

  const providerOptions = withChosen(
    [...(olderRelay ? [] : [ALL]), ...providerList.map((p) => ({ value: p.slug, label: p.name || p.slug }))],
    shownProvider,
  );
  // A request can ask for a model the list does not hold: by provider and catalog id, or one removed since.
  const known = (Array.isArray(models.data) ? models.data : []).map((m) => m.name);
  const asked = [...new Set((rows ?? []).map((r) => r.model))].filter((name) => name && !known.includes(name)).sort();
  const modelOptions = withChosen([ALL, ...[...known, ...asked].map((name) => ({ value: name, label: name }))], model);
  const keyOptions = withChosen(
    [ALL, ...(Array.isArray(keys.data) ? keys.data : []).map((k) => ({ value: k.id, label: k.name || k.id }))],
    gatewayKey,
  );

  return (
    <div className="inspector-page">
      {header}
      {olderRelay && <p className="muted small">This relay lists requests one provider at a time.</p>}
      {/* A failed refresh keeps what was loaded on screen; without this nothing would say it is no longer current. */}
      {list.isError && list.data !== undefined && (
        <ErrorNotice variant="warn" role="status">
          The list could not be refreshed. It shows what was loaded last.
        </ErrorNotice>
      )}
      <LogView<AiCapturedRequest>
        label="Requests"
        rows={rows}
        rowKey={(r) => `${r.service_id}/${r.id}`}
        columns={columns}
        // Rows of the previous filter stay while the new ones load; an empty list does
        // not, because its text would speak for a filter the relay has not answered yet,
        // nor before it is known whether capture is switched off.
        isLoading={
          providers.isLoading || resolving || list.isLoading
          || (list.isPlaceholderData && list.data?.length === 0)
          || (isEmpty && configs.some((c) => c.isLoading))
        }
        error={failed.error}
        onRetry={() => void failed.refetch()}
        range={range}
        onRangeChange={setRange}
        search={search}
        onSearchChange={(s) => setParam("q", s)}
        filters={
          <>
            <Picker label="Provider" value={shownProvider} options={providerOptions} onChange={setProvider} />
            {!olderRelay && <Picker label="Model" value={model} options={modelOptions} onChange={(v) => setParam("model", v)} />}
            {!olderRelay && <Picker label="Gateway key" value={gatewayKey} options={keyOptions} onChange={(v) => setParam("gateway_key", v)} />}
          </>
        }
        empty={empty}
      />
      {(rows?.length ?? 0) >= LIMIT && (
        <p className="muted small">
          Showing the newest {LIMIT} requests that match. Use the filter to find older ones.
        </p>
      )}
    </div>
  );
}
