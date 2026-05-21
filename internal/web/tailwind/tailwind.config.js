// Tailwind config for the toggle-monitor UI. Scans templ-generated Go
// files (and any handwritten HTML/templ sources) for utility classes.
// Run via `make tailwind`.
/** @type {import('tailwindcss').Config} */
module.exports = {
  content: [
    "internal/web/templates/**/*.templ",
    "internal/web/templates/**/*_templ.go",
  ],
  theme: {
    extend: {},
  },
  plugins: [],
};
