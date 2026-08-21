// Package web embeds noctune's HTML templates and static assets (htmx,
// its SSE extension, and the stylesheet) into the compiled binary, so the
// server ships and deploys as a single self-contained executable with no
// separate asset directory to mount.
package web

import "embed"

//go:embed templates/*.html
var Templates embed.FS

//go:embed static
var Static embed.FS
