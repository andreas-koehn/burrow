import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Circle, CircleCheck } from "lucide-react";
import { cx } from "@/components/ds";

export interface ChecklistStep {
  id: string;
  title: string;
  /** One line: what the step is for. */
  description: string;
  done: boolean;
  action?: { label: string; to: string };
  /** Shown in the open step, above the action. */
  content?: ReactNode;
}

export interface SetupChecklistProps {
  title: string;
  steps: ChecklistStep[];
}

/**
 * What is left to set up, as an ordered list. The first open step is expanded;
 * once every step is done the list has nothing to say and renders nothing.
 */
export function SetupChecklist({ title, steps }: SetupChecklistProps) {
  const uid = useId();
  // Only what the reader toggled by hand; everything else follows the steps.
  const [toggled, setToggled] = useState<Record<string, boolean>>({});
  const done = steps.filter((s) => s.done).length;
  if (done === steps.length) return null;
  const firstOpen = steps.find((s) => !s.done)?.id;

  return (
    <section className="card setup-checklist" aria-labelledby={`${uid}-title`}>
      <div className="setup-checklist-head">
        <h2 id={`${uid}-title`}>{title}</h2>
        <span className="muted small">{done} of {steps.length} done</span>
      </div>
      {/* role stated outright: without list markers Safari drops the list semantics. */}
      <ol role="list" aria-labelledby={`${uid}-title`}>
        {steps.map((s) => {
          // A step with neither content nor action has nothing to disclose.
          const expandable = s.content != null || s.action != null;
          const open = expandable && (toggled[s.id] ?? s.id === firstOpen);
          const panelId = `${uid}-${s.id}`;
          const Marker = s.done ? CircleCheck : Circle;
          return (
            <li key={s.id} className={cx("setup-step", s.done && "is-done")}>
              <span className="setup-step-marker">
                <Marker size={16} aria-hidden="true" />
                <span className="visually-hidden">{s.done ? "done" : "to do"}</span>
              </span>
              <div className="setup-step-body">
                {expandable ? (
                  <button
                    type="button"
                    className="setup-step-title"
                    aria-expanded={open}
                    aria-controls={panelId}
                    onClick={() => setToggled((t) => ({ ...t, [s.id]: !open }))}
                  >
                    {s.title}
                  </button>
                ) : (
                  <span className="setup-step-title">{s.title}</span>
                )}
                <p className="muted small">{s.description}</p>
                {expandable && (
                  <div id={panelId} className="setup-step-panel" hidden={!open}>
                    {open && s.content}
                    {open && s.action && <Link to={s.action.to}>{s.action.label}<span aria-hidden="true"> →</span></Link>}
                  </div>
                )}
              </div>
            </li>
          );
        })}
      </ol>
    </section>
  );
}
