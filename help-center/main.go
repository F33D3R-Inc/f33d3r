package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Doc struct {
	DisplayName string
	Filename    string
}

func scanDocs() ([]Doc, error) {
	entries, err := os.ReadDir("./docs")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var docs []Doc
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.ToLower(filepath.Ext(name)) != ".docx" {
			continue
		}
		display := strings.TrimSuffix(name, filepath.Ext(name))
		display = strings.ReplaceAll(display, "_", " ")
		docs = append(docs, Doc{DisplayName: display, Filename: name})
	}
	return docs, nil
}

func renderDocList(docs []Doc, tmpl *template.Template, w http.ResponseWriter) {
	if err := tmpl.ExecuteTemplate(w, "doclist", docs); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		log.Printf("doclist template error: %v", err)
	}
}

func main() {
	tmpl, err := template.ParseFiles("./templates/index.html")
	if err != nil {
		log.Fatalf("failed to parse templates: %v", err)
	}

	// GET / — full page
	http.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		docs, err := scanDocs()
		if err != nil {
			log.Printf("scanDocs error: %v", err)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "index", docs); err != nil {
			log.Printf("index template error: %v", err)
		}
	})

	// GET /docs/list — HTMX fragment: just the <ul>
	http.HandleFunc("GET /docs/list", func(w http.ResponseWriter, r *http.Request) {
		docs, err := scanDocs()
		if err != nil {
			log.Printf("scanDocs error: %v", err)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		renderDocList(docs, tmpl, w)
	})

	// GET /docs/{filename} — download a .docx file
	http.HandleFunc("GET /docs/", func(w http.ResponseWriter, r *http.Request) {
		// Strip the /docs/ prefix
		raw := strings.TrimPrefix(r.URL.Path, "/docs/")
		if raw == "" || raw == "list" {
			http.NotFound(w, r)
			return
		}

		// Sanitize: take base name only, no path traversal
		filename := filepath.Base(raw)

		// Only allow .docx
		if strings.ToLower(filepath.Ext(filename)) != ".docx" {
			http.NotFound(w, r)
			return
		}

		fpath := filepath.Join("./docs", filename)

		// Confirm it exists and is a regular file
		info, err := os.Stat(fpath)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
		http.ServeFile(w, r, fpath)
	})

	addr := ":8080"
	log.Printf("help-center listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
