package web

import (
	"embed"
	"html/template"
	"io/fs"
	"sync"
)

//go:embed *.html app.css
var content embed.FS

//go:embed layout.html metrics.html settings.html users.html
var pageTemplates embed.FS

var (
	tmpl *template.Template
	once sync.Once
)

// Templates returns the parsed HTML templates for the UI, embedded at build time.
// The layout.html template includes component templates (metrics.html, settings.html, users.html)
// via the "content" template block.
func Templates() *template.Template {
	once.Do(func() {
		tmpl = template.Must(template.ParseFS(content, "*.html"))
	})
	return tmpl
}

// StaticFS exposes embedded static assets such as CSS.
func StaticFS() fs.FS {
	return content
}
