// Package web serves the read-only UI, k8s probe endpoints, and the
// Prometheus metrics endpoint. Templates (templ-generated Go), static
// assets, and Tailwind output CSS are embedded via embed.FS.
package web

import (
	"embed"

	// templ runtime is locked here; generated components land alongside
	// the UI work in Issue 2 and beyond.
	_ "github.com/a-h/templ"
)

// StaticAssets holds the embedded static asset tree (CSS and any other
// small static files). Tailwind compiles into static/css/app.css.
//
//go:embed all:static
var StaticAssets embed.FS
