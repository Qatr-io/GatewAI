package ui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed templates/* static/*
var assetsFS embed.FS

// StaticFS returns an http.FileSystem serving /static/* from embedded assets.
func StaticFS() fs.FS {
	sub, _ := fs.Sub(assetsFS, "static")
	return sub
}

func parseTemplates() (*template.Template, error) {
	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"int": func(v int64) int { return int(v) },
		"isLastRow": func(e any, events any, page, limit int, total int64) bool {
			return false // simplified; infinite-scroll sentinel handled per-page
		},
	}
	return template.New("").Funcs(funcMap).ParseFS(assetsFS,
		"templates/base.html",
		"templates/dashboard.html",
		"templates/history.html",
		"templates/admin.html",
		"templates/admin_consumer.html",
		"templates/partials/quota_cards.html",
		"templates/partials/history_rows.html",
	)
}

// StaticHandler returns an http.Handler for /static/* embedded assets.
func StaticHandler() http.Handler {
	return http.FileServer(http.FS(StaticFS()))
}
