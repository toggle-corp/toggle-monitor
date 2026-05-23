// Tailwind config for the toggle-monitor UI. Scans every .templ source
// + every .go file under templates/ (covers both the templ-generated
// *_templ.go and hand-written helpers like helpers.go that build
// classnames as Go string literals — e.g. discoveryBadgeClasses).
// Run via `make tailwind`.
/** @type {import('tailwindcss').Config} */
module.exports = {
  darkMode: "class",
  content: [
    "internal/web/templates/**/*.templ",
    "internal/web/templates/**/*.go",
  ],
  theme: {
    extend: {},
  },
  plugins: [],
};
