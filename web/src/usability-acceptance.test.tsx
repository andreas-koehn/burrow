// usability-acceptance.test.tsx
//
// Acceptance skeleton for the Burrow "5-star usability" plan.
//
// Each it.todo() below will be converted to a real assertion — typically
//   renderApp(<App />, { route: "/some/path" }) + screen.getBy…()
// — as the corresponding implementation phase lands. Until then, it.todo()
// marks the criterion as PENDING (not failing) so the suite stays green.
//
// Journey/colour/screenshot criteria that require a real browser live in
// web/e2e/usability.spec.ts (Playwright against the Docker Compose stack).
//
// Do NOT add real assertions for unbuilt features here; they would fail.

import { describe, it } from "vitest";

// ---------------------------------------------------------------------------
// INTUITIVE
// ---------------------------------------------------------------------------
describe("INTUITIVE — navigation & mental-model alignment", () => {
  it.todo("In-1: / (root route) renders Home/Dashboard, not a blank page or redirect loop");
  it.todo("In-2: primary nav lists Home as the first entry");
  it.todo("In-3: Home page contains a 'How Burrow works' explainer section");
  it.todo("In-4: clicking a Clients cross-link navigates to the correct client detail view");
  it.todo("In-4: clicking a Services cross-link navigates to the correct service detail view");
  it.todo("In-4: clicking a Tunnels cross-link navigates to the correct tunnel detail view");
  it.todo("In-5: Token detail page includes a cross-link to the owning service/client entity");
});

// ---------------------------------------------------------------------------
// CLEAR
// ---------------------------------------------------------------------------
describe("CLEAR — information architecture & labelling", () => {
  it.todo("Clr-1: Tunnels page shows a live-vs-config explainer (count badge or descriptive label)");
  it.todo("Clr-2: Services page shows a live-vs-config explainer");
  it.todo("Clr-4: AI-endpoints empty state renders a '+Create AI service' CTA button");
  it.todo("Clr-6: 'Custom domains' label appears exactly once per section in Settings");
});

// ---------------------------------------------------------------------------
// EASY
// ---------------------------------------------------------------------------
describe("EASY — onboarding & discoverability", () => {
  it.todo("Ea-4: Users page renders an SMTP-not-configured notice with a link to Settings > SMTP");
  it.todo("Ea-5: Tunnels empty state has at least one CTA button");
  it.todo("Ea-5: Services empty state has at least one CTA button");
  it.todo("Ea-5: Clients empty state has at least one CTA button");
  it.todo("Ea-5: AI Endpoints empty state has at least one CTA button");
  it.todo("Ea-5: Tokens empty state has at least one CTA button");
});
