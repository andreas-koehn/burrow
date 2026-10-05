/**
 * How to turn request capture on for a service. No dashboard page writes this setting,
 * so the hint names the API call rather than sending the reader to a page without a switch.
 */
export function InspectorOffHint({ serviceId }: { serviceId: string }) {
  return (
    <span>
      The dashboard has no switch for this yet. Set <code className="mono">inspector.enabled</code> to{" "}
      <code className="mono">true</code> with{" "}
      <code className="mono">PUT /api/v1/services/{serviceId}/ai-config</code>. The call stores the whole
      configuration, so send the current one with that field changed.
    </span>
  );
}
