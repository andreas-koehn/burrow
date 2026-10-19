import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { X } from "lucide-react";
import { apiFetch } from "@/lib/api";
import { Button, Input, Select, Switch } from "@/components/ds";
import { MAX_TOOL_ENTRIES, toolEntryProblem } from "@/lib/toolAllow";
import type { ToolSource } from "@/lib/contract";

export interface KeyToolsFieldProps {
  /** Prefix for the ids of the controls, so two dialogs never share one. */
  idPrefix: string;
  value: string[];
  onChange: (value: string[]) => void;
  readOnly: boolean;
  onReadOnlyChange: (value: boolean) => void;
}

/**
 * The tools a gateway key may call: a source picker that adds "<source>__*", a free
 * pattern box, the entries as removable chips, and the read-only switch. An empty list
 * means no tools at all, the opposite of the model list, and the field says so.
 */
export function KeyToolsField({ idPrefix, value, onChange, readOnly, onReadOnlyChange }: KeyToolsFieldProps) {
  const noteId = useId();
  const errId = useId();
  const [source, setSource] = useState("");
  const [pattern, setPattern] = useState("");
  const [problem, setProblem] = useState("");

  // Same key and fetch as the Tools page. A reader without mcp:sources:read gets 403:
  // the picker is then absent and the pattern box still works.
  const sources = useQuery({
    queryKey: ["mcp", "sources"],
    queryFn: () => apiFetch<ToolSource[]>("/mcp/sources"),
    retry: false,
  });
  const list = Array.isArray(sources.data) ? sources.data : [];

  function add(entry: string): boolean {
    if (value.includes(entry)) return true;
    if (value.length >= MAX_TOOL_ENTRIES) {
      setProblem(`A key lists at most ${MAX_TOOL_ENTRIES} tool entries.`);
      return false;
    }
    onChange([...value, entry]);
    return true;
  }

  function addPattern() {
    const entry = pattern.trim();
    if (entry === "") return;
    const bad = toolEntryProblem(entry);
    if (bad) { setProblem(bad); return; }
    setProblem("");
    if (add(entry)) setPattern("");
  }

  return (
    <fieldset className="col gap-2 key-tools">
      <legend className="muted small">Tools the key may call</legend>
      {list.length > 0 && (
        <div className="row row-center gap-2">
          <label htmlFor={`${idPrefix}-source`} className="visually-hidden">Tool source</label>
          <Select
            id={`${idPrefix}-source`}
            options={list.map((s) => ({ value: s.slug, label: s.slug }))}
            value={source}
            placeholder="Tool source…"
            onChange={setSource}
          />
          <Button
            variant="secondary"
            size="sm"
            disabled={source === ""}
            onClick={() => { if (add(`${source}__*`)) { setSource(""); setProblem(""); } }}
          >
            Add all tools of source
          </Button>
        </div>
      )}
      <div className="row row-center gap-2">
        <label htmlFor={`${idPrefix}-pattern`} className="visually-hidden">Tool pattern</label>
        <Input
          id={`${idPrefix}-pattern`}
          className="fill-rest mono"
          value={pattern}
          placeholder="github__list_issues or github__*"
          autoComplete="off"
          spellCheck={false}
          aria-invalid={problem !== "" || undefined}
          aria-describedby={problem !== "" ? errId : noteId}
          onChange={(e) => { setPattern(e.target.value); setProblem(""); }}
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); addPattern(); } }}
        />
        <Button variant="secondary" size="sm" onClick={addPattern}>Add pattern</Button>
      </div>
      {problem !== "" && <p id={errId} role="alert" className="notice-inline error">{problem}</p>}
      {value.length > 0 ? (
        <ul className="key-tool-chips" aria-label="Allowed tools">
          {value.map((v) => (
            <li key={v} className="key-tool-chip mono small">
              <span>{v}</span>
              <button type="button" className="icon-btn" aria-label={`Remove ${v}`} onClick={() => onChange(value.filter((x) => x !== v))}>
                <X size={12} aria-hidden="true" />
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <p id={noteId} className="muted small">No tools selected. A key without tools cannot call any tool.</p>
      )}
      {value.length > 0 && <span id={noteId} className="visually-hidden">Only the listed tools can be called.</span>}
      <div className="row row-center gap-2">
        <Switch id={`${idPrefix}-readonly`} checked={readOnly} onChange={onReadOnlyChange} aria-label="Read-only tools only" />
        <label htmlFor={`${idPrefix}-readonly`}>Read-only tools only</label>
      </div>
      <p className="muted small">
        An entry is one tool, <code>&lt;source&gt;__&lt;tool&gt;</code>, or every tool of a source, <code>&lt;source&gt;__*</code>.
        With the switch on, the key reaches only tools their operator classed as read-only.
      </p>
    </fieldset>
  );
}
