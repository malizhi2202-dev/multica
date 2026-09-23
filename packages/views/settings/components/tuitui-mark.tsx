// TuituiMark is the channel glyph for Tuitui (推推). The platform publishes no
// SVG asset or brand-icon entry (lucide ships no brand icons, Simple Icons has
// no tuitui entry), so this is an original mark drawn for Multica: a speech
// bubble with an upward "push" arrow, echoing the product name (推 = push).
// Uses currentColor so it themes like the other channel marks.
export function TuituiMark({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      aria-hidden="true"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.4z" />
      <path d="M12 15.5v-6" />
      <path d="m9.2 12 2.8-2.8 2.8 2.8" />
    </svg>
  );
}
