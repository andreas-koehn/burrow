import type { ReactNode } from "react";
import { Badge } from "@/components/ds";
import { diffWords } from "@/lib/textDiff";
import type { ToolDefinition } from "@/lib/contract";

export interface ToolDefinitionViewProps {
  /** The group's name for a screen reader: "Approved definition" or "New definition". */
  label: string;
  definition: ToolDefinition;
  /** The other definition: words that differ from it are marked. */
  other?: ToolDefinition | null;
  /** Which side this is: the words only it has are removed ones on the old side, added ones on the new side. */
  side: "old" | "new";
}

const json = (v: unknown) => JSON.stringify(v, null, 2);

function Block({ title, value, was }: { title: string; value: unknown; was?: { value: unknown } }) {
  if (value === undefined || value === null) return null;
  const changed = was !== undefined && json(was.value) !== json(value);
  return (
    <div data-changed={changed ? title : undefined}>
      <div className="section-label">{title}{changed && <Badge kind="status-idle" nodot className="diff-badge">changed</Badge>}</div>
      {/* Focusable so the keyboard can scroll a long schema. */}
      <pre className="cmd-block" tabIndex={0} role="group" aria-label={title}><code>{json(value)}</code></pre>
    </div>
  );
}

/** One definition of a tool, as text: description (differences marked), schemas and annotations as formatted JSON. */
export function ToolDefinitionView({ label, definition, other, side }: ToolDefinitionViewProps) {
  const description = definition.description ?? "";
  let text: ReactNode = description === "" ? <span className="muted">No description.</span> : description;
  if (other) {
    const otherText = other.description ?? "";
    const parts = side === "new" ? diffWords(otherText, description) : diffWords(description, otherText);
    const mine = side === "new" ? "add" : "del";
    const theirs = side === "new" ? "del" : "add";
    if (description !== "" || otherText !== "") {
      text = parts.filter((p) => p.kind !== theirs).map((p, i) => {
        if (p.kind !== mine) return <span key={i}>{p.text}</span>;
        return side === "new" ? <ins key={i} className="diff-add">{p.text}</ins> : <del key={i} className="diff-del">{p.text}</del>;
      });
    }
  }
  const titleChanged = other !== undefined && other !== null && (other.title ?? "") !== (definition.title ?? "");
  return (
    <div role="group" aria-label={label} className="col gap-2 tool-definition">
      <div className="section-label">{label}</div>
      {definition.title !== undefined && definition.title !== "" && (
        <div><strong>{definition.title}</strong>{titleChanged && <span className="muted small"> (title changed)</span>}</div>
      )}
      {/* The text is the upstream's, shown as text: never as markup. */}
      <p className="tool-description" style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere", margin: 0 }}>{text}</p>
      <Block title="Input schema" value={definition.inputSchema} was={side === "new" && other ? { value: other.inputSchema } : undefined} />
      <Block title="Output schema" value={definition.outputSchema} was={side === "new" && other ? { value: other.outputSchema } : undefined} />
      <Block title="Annotations" value={definition.annotations} was={side === "new" && other ? { value: other.annotations } : undefined} />
    </div>
  );
}
