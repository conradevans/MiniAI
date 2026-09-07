package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed frontend/*
var frontendFiles embed.FS

func (a *app) registerWebRoutes(mux *http.ServeMux) {
	assets, err := fs.Sub(frontendFiles, "frontend")
	if err != nil {
		panic(err)
	}

	fileServer := http.FileServer(http.FS(assets))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", fileServer))

	index := func(w http.ResponseWriter, r *http.Request) {
		data, err := frontendFiles.ReadFile("frontend/index.html")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "MiniAI frontend unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		_, _ = w.Write(data)
	}

	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /admin", index)
	mux.HandleFunc("GET /admin/", index)
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		data, err := frontendFiles.ReadFile("frontend/favicon.svg")
		if err != nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(data)
	})

}
