import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import {
  Button, Dialog, FormField, FormFieldGroup, Input, PageHeader, Select, SkeletonRows, Switch, Tabs,
} from "@/components/ds";
import type { CacheSettings, SemanticCacheSettings, CacheStatsV5, Service, ServiceAIConfig } from "@/lib/contract";
import { withAIConfigDefaults } from "@/lib/aiConfig";
import { formatBytes } from "@/lib/format";

interface CacheSettingsPayload {
  global: CacheSettings;
  per_service: Record<string, CacheSettings>;
  semantic?: SemanticCacheSettings;
}

const APPLIES_OPTIONS = [
  { value: "global", label: "Global (cache shared across providers)" },
  { value: "per_endpoint", label: "Per-provider" },
  { value: "per_api_key", label: "Per-API-key" },
];

const EMBEDDING_MODE_OPTIONS = [
  { value: "local", label: "Local" },
  { value: "none", label: "Off" },
];

const FALLBACK_POLICY_OPTIONS = [
  { value: "treat_as_miss", label: "Treat as miss (default)" },
  { value: "return_cached_marked", label: "Return cached + mark" },
];

const DEFAULT_SEMANTIC: SemanticCacheSettings = {
  enabled: false,
  min_similarity: 0.85,
  embedding_mode: "local",
  embedding_url: "http://localhost:11434/v1/embeddings",
  embedding_model: "nomic-embed-text",
  fallback_policy: "treat_as_miss",
  promote_on_miss: true,
  max_index_entries: 10000,
};

function ExactPanel({
  draft, setDraft, isSaving, onSave,
}: {
  draft: CacheSettings;
  setDraft: (next: CacheSettings) => void;
  isSaving: boolean;
  onSave: () => void;
}) {
  return (
    <div className="exact-cache">
      <div className="row row-center gap-2">
        <Switch
          aria-label="Enable exact cache"
          checked={draft.enabled}
          onChange={(v) => setDraft({ ...draft, enabled: v })}
        />
        <span>Cache exact (verbatim) requests</span>
      </div>
      <FormFieldGroup>
        <FormField label="Applies per" htmlFor="cache-applies" w="md">
          <Select
            id="cache-applies"
            value={draft.applies_per}
            onChange={(v) => setDraft({ ...draft, applies_per: v as CacheSettings["applies_per"] })}
            options={APPLIES_OPTIONS}
          />
        </FormField>
        <FormField label="TTL (seconds)" htmlFor="cache-ttl" w="sm">
          <Input
            id="cache-ttl"
            type="number"
            className="mono"
            min={0}
            value={draft.ttl_seconds}
            onChange={(e) => setDraft({ ...draft, ttl_seconds: Number(e.target.value) })}
          />
        </FormField>
        <FormField label="Max entries" htmlFor="cache-max-entries" w="sm">
          <Input
            id="cache-max-entries"
            type="number"
            className="mono"
            min={1}
            value={draft.max_entries}
            onChange={(e) => setDraft({ ...draft, max_entries: Number(e.target.value) })}
          />
        </FormField>
        <FormField label="Max per-entry (KiB)" htmlFor="cache-max-per-entry" w="sm">
          <Input
            id="cache-max-per-entry"
            type="number"
            className="mono"
            min={1}
            value={draft.max_per_entry_kb}
            onChange={(e) => setDraft({ ...draft, max_per_entry_kb: Number(e.target.value) })}
          />
        </FormField>
      </FormFieldGroup>
      <div className="actions">
        <Button variant="primary" size="sm" disabled={isSaving} onClick={onSave}>
          {isSaving ? "Saving…" : "Save cache settings"}
        </Button>
      </div>
    </div>
  );
}

/**
 * The relay sends the rate without the number of lookups behind it. An empty
 * cache that hit nothing has measured nothing, so it gets a dash, not 0.0%.
 */
function semanticHitRate(stats: CacheStatsV5): string {
  if (!(stats.semantic_entries > 0) && !(stats.semantic_hit_rate_24h > 0)) return "—";
  return `${(stats.semantic_hit_rate_24h * 100).toFixed(1)}%`;
}

function SemanticStatsPanel({ stats }: { stats: CacheStatsV5 }) {
  return (
    <div className="semantic-stats">
      <div className="stat-row"><span>Semantic entries</span><span>{stats.semantic_entries}</span></div>
      <div className="stat-row"><span>Semantic disk</span><span>{formatBytes(stats.semantic_disk_bytes)}</span></div>
      <div className="stat-row"><span>Semantic hit rate (24h)</span><span>{semanticHitRate(stats)}</span></div>
      <div className="stat-row"><span>Similar returned (24h)</span><span>{stats.semantic_similar_returned_24h}</span></div>
      <div className="stat-row"><span>Promotions (24h)</span><span>{stats.semantic_promotions_24h}</span></div>
    </div>
  );
}

function SemanticPanel() {
  const qc = useQueryClient();

  // Fetch all services so we can offer a per-service picker
  const servicesQuery = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });

  // AI services — services with access_mode === "api_key"
  const aiServices = (servicesQuery.data ?? []).filter((s) => s.access_mode === "api_key");
  const [selectedServiceId, setSelectedServiceId] = useState<string>("");

  // Once services load, default to the first AI service
  useEffect(() => {
    if (!selectedServiceId && aiServices.length > 0) {
      setSelectedServiceId(aiServices[0]!.id);
    }
  }, [aiServices, selectedServiceId]);

  // Fetch the per-service AI config for the selected service
  const aiConfigQuery = useQuery({
    queryKey: ["service", selectedServiceId, "ai-config"],
    queryFn: () =>
      apiFetch<Partial<ServiceAIConfig>>(`/services/${selectedServiceId}/ai-config`).then(withAIConfigDefaults),
    enabled: !!selectedServiceId,
    retry: false,
  });

  // Fetch cache stats (CacheStatsV5)
  const statsQuery = useQuery({
    queryKey: ["cache", "stats"],
    queryFn: () => apiFetch<CacheStatsV5>("/cache/stats"),
    retry: false,
  });

  // Draft semantic settings — initialized from the per-service ai-config
  const [semanticDraft, setSemanticDraft] = useState<SemanticCacheSettings | null>(null);

  useEffect(() => {
    if (aiConfigQuery.data && !semanticDraft) {
      setSemanticDraft(aiConfigQuery.data.cache.semantic ?? { ...DEFAULT_SEMANTIC });
    }
  }, [aiConfigQuery.data, semanticDraft]);

  // When the service picker changes, reset the draft so we re-initialize from the new config
  const handleServiceChange = (id: string) => {
    setSelectedServiceId(id);
    setSemanticDraft(null);
  };

  const save = useMutation({
    mutationFn: () => {
      if (!selectedServiceId || !aiConfigQuery.data || !semanticDraft) {
        return Promise.reject(new Error("Not ready"));
      }
      const payload: ServiceAIConfig = {
        ...aiConfigQuery.data,
        cache: {
          ...aiConfigQuery.data.cache,
          semantic: semanticDraft,
        },
      };
      return apiFetch<void>(`/services/${selectedServiceId}/ai-config`, {
        method: "PUT",
        body: JSON.stringify(payload),
      });
    },
    onSuccess: () => {
      toast.success("Semantic cache settings saved.");
      qc.invalidateQueries({ queryKey: ["service", selectedServiceId, "ai-config"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't save semantic cache settings."),
  });

  const draft = semanticDraft;
  const simOutOfRange =
    draft !== null &&
    draft.enabled &&
    (draft.min_similarity < 0 || draft.min_similarity > 1);

  const serviceOptions = aiServices.map((s) => ({ value: s.id, label: s.name }));

  return (
    <div className="semantic-cache">
      <p className="muted">
        Semantic caching is off by default. Enable only after reading the docs — vector
        similarity can return stale or near-miss answers.
      </p>

      {statsQuery.data && <SemanticStatsPanel stats={statsQuery.data} />}

      {serviceOptions.length > 0 && (
        <FormField label="Service" htmlFor="semantic-service-picker" w="md">
          <Select
            id="semantic-service-picker"
            value={selectedServiceId}
            onChange={handleServiceChange}
            options={serviceOptions}
          />
        </FormField>
      )}

      <div className="row row-center gap-2">
        <Switch
          aria-label="Enable semantic cache"
          checked={draft?.enabled ?? false}
          onChange={(v) => draft && setSemanticDraft({ ...draft, enabled: v })}
        />
        <span>Enable semantic cache</span>
      </div>

      {draft?.enabled && (
        <FormFieldGroup>
          <FormField label="Min similarity" htmlFor="sem-min-similarity" w="sm">
            <Input
              id="sem-min-similarity"
              type="number"
              className="mono"
              min={0}
              max={1}
              step={0.01}
              value={draft.min_similarity}
              onChange={(e) =>
                setSemanticDraft({ ...draft, min_similarity: Number(e.target.value) })
              }
            />
          </FormField>
          <FormField label="Embedding mode" htmlFor="sem-embedding-mode" w="md">
            <Select
              id="sem-embedding-mode"
              value={draft.embedding_mode}
              onChange={(v) =>
                setSemanticDraft({ ...draft, embedding_mode: v as SemanticCacheSettings["embedding_mode"] })
              }
              options={EMBEDDING_MODE_OPTIONS}
            />
          </FormField>
          <FormField label="Embedding URL" htmlFor="sem-embedding-url" w="md">
            <Input
              id="sem-embedding-url"
              className="mono"
              value={draft.embedding_url}
              onChange={(e) => setSemanticDraft({ ...draft, embedding_url: e.target.value })}
            />
          </FormField>
          <FormField label="Embedding model" htmlFor="sem-embedding-model" w="md">
            <Input
              id="sem-embedding-model"
              className="mono"
              value={draft.embedding_model}
              onChange={(e) => setSemanticDraft({ ...draft, embedding_model: e.target.value })}
            />
          </FormField>
          <FormField label="Fallback policy" htmlFor="sem-fallback-policy" w="md">
            <Select
              id="sem-fallback-policy"
              value={draft.fallback_policy}
              onChange={(v) =>
                setSemanticDraft({ ...draft, fallback_policy: v as SemanticCacheSettings["fallback_policy"] })
              }
              options={FALLBACK_POLICY_OPTIONS}
            />
          </FormField>
          <div className="row row-center gap-2">
            <Switch
              aria-label="Promote on miss"
              checked={draft.promote_on_miss}
              onChange={(v) => setSemanticDraft({ ...draft, promote_on_miss: v })}
            />
            <span>Promote on miss</span>
          </div>
          <FormField label="Max index entries" htmlFor="sem-max-index-entries" w="sm">
            <Input
              id="sem-max-index-entries"
              type="number"
              className="mono"
              min={1}
              value={draft.max_index_entries}
              onChange={(e) =>
                setSemanticDraft({ ...draft, max_index_entries: Number(e.target.value) })
              }
            />
          </FormField>
        </FormFieldGroup>
      )}

      {simOutOfRange && (
        <p role="alert" className="notice-inline error">
          Similarity must be between 0 and 1.
        </p>
      )}

      <div className="actions">
        <Button
          variant="primary"
          size="sm"
          disabled={save.isPending || simOutOfRange || !draft}
          onClick={() => save.mutate()}
        >
          {save.isPending ? "Saving…" : "Save semantic settings"}
        </Button>
      </div>
    </div>
  );
}

export default function PromptCache() {
  const qc = useQueryClient();
  const settings = useQuery({
    queryKey: ["cache", "settings"],
    queryFn: () => apiFetch<CacheSettingsPayload>("/cache/settings"),
    retry: false,
  });
  const [draft, setDraft] = useState<CacheSettings | null>(null);
  useEffect(() => {
    if (settings.data && !draft) setDraft(settings.data.global);
  }, [settings.data, draft]);

  const save = useMutation({
    mutationFn: () =>
      apiFetch<void>("/cache/settings", {
        method: "PUT",
        body: JSON.stringify({
          global: draft,
          per_service: settings.data?.per_service ?? {},
        }),
      }),
    onSuccess: () => {
      toast.success("Cache settings saved.");
      qc.invalidateQueries({ queryKey: ["cache", "settings"] });
    },
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't save cache settings."),
  });

  const clear = useMutation({
    mutationFn: () => apiFetch<void>("/cache/entries", { method: "DELETE" }),
    onSuccess: () => toast.success("Cache cleared."),
    onError: (e: unknown) =>
      toast.error(e instanceof ApiError ? e.message : "Couldn't clear cache."),
  });

  const [tab, setTab] = useState("exact");
  const [clearOpen, setClearOpen] = useState(false);

  if (!draft) {
    return (
      <div className="prompt-cache-page">
        <PageHeader title="Prompt cache" />
        <SkeletonRows n={4} />
      </div>
    );
  }

  return (
    <div className="prompt-cache-page">
      <PageHeader
        title="Prompt cache"
        subtitle="The cache lives on this relay. No data is sent anywhere."
        actions={<Button variant="secondary" size="sm" onClick={() => setClearOpen(true)}>Clear cache</Button>}
      />

      <Tabs
        value={tab}
        onChange={setTab}
        tabs={[
          { value: "exact", label: "Exact match", content: (
              <ExactPanel
                draft={draft}
                setDraft={setDraft}
                isSaving={save.isPending}
                onSave={() => save.mutate()}
              />
            ) },
          { value: "semantic", label: "Semantic", content: <SemanticPanel /> },
        ]}
      />

      <Dialog
        open={clearOpen}
        onOpenChange={setClearOpen}
        title="Clear cache?"
        footer={
          <>
            <Button variant="secondary" onClick={() => setClearOpen(false)}>Cancel</Button>
            <Button
              variant="destructive"
              onClick={() => { clear.mutate(); setClearOpen(false); }}
            >
              Clear
            </Button>
          </>
        }
      >
        <p>Clear all cached responses? This cannot be undone.</p>
      </Dialog>
      <Toaster />
    </div>
  );
}
