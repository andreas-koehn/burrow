/* Brand mark — geometric tunnel-and-arrow glyph (currentColor, never Signal Teal). */
export function BurrowMark({ size = 20 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="3" y="5.5" width="12" height="13" rx="2.5" />
      <path d="M9 12h11.5" />
      <path d="M17 9l3.5 3-3.5 3" />
    </svg>
  );
}
