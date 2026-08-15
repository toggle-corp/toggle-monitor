// Tailwind config for the toggle-monitor UI. Scans every .templ source
// + every .go file under templates/ (covers both the templ-generated
// *_templ.go and hand-written helpers like helpers.go that build
// classnames as Go string literals — e.g. TraceActionClass in
// discovery_view.go).
//
// Every color resolves to a --tm-* semantic role defined in input.css, so
// `bg-surface` / `text-muted` mean the same thing in both themes and no
// `dark:` variant is needed. Because the values are CSS variables rather
// than literal colors, Tailwind's `/opacity` modifier does not apply to
// them — reach for the *-soft roles instead.
//
// Spacing and radii need no remapping: the design system's 8/12/16/24/32/48
// spacing rhythm is exactly Tailwind's 2/3/4/6/8/12, and its 4/6/8/999px
// radii are exactly rounded / rounded-md / rounded-lg / rounded-full.
//
// Run via `just tailwind`.
/** @type {import('tailwindcss').Config} */
module.exports = {
  darkMode: "class",
  content: [
    "internal/web/templates/**/*.templ",
    "internal/web/templates/**/*.go",
  ],
  theme: {
    extend: {
      colors: {
        // chrome
        bg: "var(--tm-bg)",
        surface: "var(--tm-surface)",
        "surface-muted": "var(--tm-surface-muted)",
        "surface-hover": "var(--tm-surface-hover)",
        "surface-active": "var(--tm-surface-active)",
        border: "var(--tm-border)",
        "border-strong": "var(--tm-border-strong)",
        hair: "var(--tm-hair)",
        "code-bg": "var(--tm-code-bg)",
        // text
        ink: "var(--tm-text)",
        "ink-hover": "var(--tm-ink-hover)",
        "ink-active": "var(--tm-ink-active)",
        muted: "var(--tm-muted)",
        faint: "var(--tm-faint)",
        dim: "var(--tm-dim)",
        // accent — the only non-neutral chrome color, once per view
        accent: "var(--tm-accent)",
        "accent-strong": "var(--tm-accent-strong)",
        "accent-soft": "var(--tm-accent-soft)",
        "accent-border": "var(--tm-accent-border)",
        // status vocabulary: readable text tone + saturated dot/mark + soft wash
        up: "var(--tm-up)",
        "up-mark": "var(--tm-up-mark)",
        "up-soft": "var(--tm-success-soft)",
        down: "var(--tm-down)",
        "down-mark": "var(--tm-down-mark)",
        "down-soft": "var(--tm-error-soft)",
        warn: "var(--tm-warn)",
        "warn-mark": "var(--tm-warn-mark)",
        "warn-soft": "var(--tm-warning-soft)",
        idle: "var(--tm-idle)",
        "idle-soft": "var(--tm-idle-soft)",
      },
      fontFamily: {
        sans: ["var(--tm-font-sans)"],
        mono: ["var(--tm-font-mono)"],
      },
      fontSize: {
        "2xs": "var(--tm-fs-body-2xs)",
        xs: "var(--tm-fs-body-xs)",
        sm: "var(--tm-fs-body-sm)",
        body: "var(--tm-fs-body)",
        "body-lg": "var(--tm-fs-body-lg)",
        h5: "var(--tm-fs-h5)",
        h4: "var(--tm-fs-h4)",
        h3: "var(--tm-fs-h3)",
        h2: "var(--tm-fs-h2)",
        h1: "var(--tm-fs-h1)",
      },
      boxShadow: {
        xs: "var(--tm-shadow-xs)",
      },
    },
  },
  plugins: [],
};
