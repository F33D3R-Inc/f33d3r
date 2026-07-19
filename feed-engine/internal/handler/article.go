package handler

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

// ── Slug ─────────────────────────────────────────────────────────────────────

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(title string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		s = fmt.Sprintf("article-%d", time.Now().Unix())
	}
	return s
}

// uniqueSlug returns a slug guaranteed not to exist in articles.
func (h *Handler) uniqueSlug(base, excludeID string) string {
	slug := base
	for i := 2; i < 100; i++ {
		var existing string
		err := h.db.QueryRow(
			`SELECT id FROM articles WHERE slug = $1`, slug,
		).Scan(&existing)
		if err != nil {
			return slug // not found — available
		}
		if existing == excludeID {
			return slug // same article — ok
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixMilli())
}

// ── Excerpt & render ─────────────────────────────────────────────────────────

var mdStripRe = regexp.MustCompile(`[#*_\[\]()!>~` + "`" + `]+`)

func articleExcerpt(body string) string {
	plain := mdStripRe.ReplaceAllString(body, " ")
	plain = strings.Join(strings.Fields(plain), " ")
	if len([]rune(plain)) > 220 {
		return string([]rune(plain)[:220]) + "…"
	}
	return plain
}

// ── DB helpers ────────────────────────────────────────────────────────────────

func (h *Handler) getArticle(id string) *model.Article {
	var a model.Article
	var pubAt *time.Time
	err := h.db.QueryRow(`
		SELECT a.id, a.author_id, u.handle, COALESCE(up.display_name,''), COALESCE(up.avatar_url,''),
		       a.slug, a.title, a.body, a.body_html, a.excerpt, a.cover_url,
		       a.status, a.published_at, a.view_count, a.created_at, a.updated_at
		FROM articles a
		JOIN users u ON u.id = a.author_id
		LEFT JOIN user_profiles up ON up.user_id = a.author_id
		WHERE a.id = $1 AND a.status != 'archived'`, id).
		Scan(&a.ID, &a.AuthorID, &a.AuthorHandle, &a.AuthorDisplay, &a.AuthorAvatar,
			&a.Slug, &a.Title, &a.Body, &a.BodyHTML, &a.Excerpt, &a.CoverURL,
			&a.Status, &pubAt, &a.ViewCount, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil
	}
	a.PublishedAt = pubAt
	return &a
}

func (h *Handler) getArticleBySlug(slug string) *model.Article {
	var a model.Article
	var pubAt *time.Time
	err := h.db.QueryRow(`
		SELECT a.id, a.author_id, u.handle, COALESCE(up.display_name,''), COALESCE(up.avatar_url,''),
		       a.slug, a.title, a.body, a.body_html, a.excerpt, a.cover_url,
		       a.status, a.published_at, a.view_count, a.created_at, a.updated_at
		FROM articles a
		JOIN users u ON u.id = a.author_id
		LEFT JOIN user_profiles up ON up.user_id = a.author_id
		WHERE a.slug = $1 AND a.status != 'archived'`, slug).
		Scan(&a.ID, &a.AuthorID, &a.AuthorHandle, &a.AuthorDisplay, &a.AuthorAvatar,
			&a.Slug, &a.Title, &a.Body, &a.BodyHTML, &a.Excerpt, &a.CoverURL,
			&a.Status, &pubAt, &a.ViewCount, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil
	}
	a.PublishedAt = pubAt
	return &a
}

func (h *Handler) getUserArticles(userID string) []model.Article {
	rows, err := h.db.Query(`
		SELECT id, slug, title, excerpt, status, published_at, view_count, created_at, updated_at
		FROM articles
		WHERE author_id = $1 AND status != 'archived'
		ORDER BY updated_at DESC
		LIMIT 100`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []model.Article
	for rows.Next() {
		var a model.Article
		var pubAt *time.Time
		rows.Scan(&a.ID, &a.Slug, &a.Title, &a.Excerpt, &a.Status,
			&pubAt, &a.ViewCount, &a.CreatedAt, &a.UpdatedAt)
		a.PublishedAt = pubAt
		out = append(out, a)
	}
	return out
}

// ── Page handlers ─────────────────────────────────────────────────────────────

// articlesPage lists the current user's articles (creator dashboard).
// GET /articles
func (h *Handler) articlesPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	articles := h.getUserArticles(user.ID)
	h.render(w, "articles.html", map[string]interface{}{
		"User":     user,
		"Articles": articles,
		"Title":    "My articles · F33D3R",
	})
}

// newArticlePage renders a blank editor.
// GET /articles/new
func (h *Handler) newArticlePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	h.render(w, "article_editor.html", map[string]interface{}{
		"User":    user,
		"Article": nil,
		"Title":   "New article · F33D3R",
	})
}

// editArticlePage renders the editor pre-filled with an existing article.
// GET /articles/{id}/edit
func (h *Handler) editArticlePage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	id := r.PathValue("id")
	article := h.getArticle(id)
	if article == nil || article.AuthorID != user.ID {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	h.render(w, "article_editor.html", map[string]interface{}{
		"User":    user,
		"Article": article,
		"Title":   "Edit · " + article.Title + " · F33D3R",
	})
}

// articleReaderPage renders a published article for public reading.
// GET /article/{slug}
func (h *Handler) articleReaderPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	slug := r.PathValue("slug")
	article := h.getArticleBySlug(slug)
	if article == nil {
		http.NotFound(w, r)
		return
	}
	// Drafts only visible to author.
	if article.Status == "draft" && (user == nil || user.ID != article.AuthorID) {
		http.NotFound(w, r)
		return
	}
	// Increment view count then push article_view SSE to the author.
	// One view = one push. No batching.
	go func() {
		h.db.Exec(`UPDATE articles SET view_count = view_count + 1 WHERE id = $1`, article.ID)
		var count int64
		h.db.QueryRow(`SELECT view_count FROM articles WHERE id = $1`, article.ID).Scan(&count)
		// Push to author if they're watching (e.g. analytics page open)
		var authorPIAL string
		h.db.QueryRow(`SELECT COALESCE(pial_id::text,'') FROM users WHERE id = $1`, article.AuthorID).Scan(&authorPIAL)
		if authorPIAL != "" {
			PublishToUser(authorPIAL, SSEEvent{
				Type: "article_view",
				Data: fmt.Sprintf(`<span id="article-views-%s" hx-get="/facets/article/%s/viewcount" hx-trigger="every 60s" hx-swap="outerHTML"><span class="article-byline-views">%d views</span></span>`,
					article.ID, article.ID, count),
			})
		}
	}()

	h.render(w, "article.html", map[string]interface{}{
		"User":    user,
		"Article": article,
		"Title":   article.Title + " · F33D3R",
	})
}

// ── Mutations ─────────────────────────────────────────────────────────────────

// saveArticle creates or updates an article.
// POST /api/article/save  fields: id (optional), title, body, status, cover_url
func (h *Handler) saveArticle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	_ = r.ParseForm()

	id       := strings.TrimSpace(r.FormValue("id"))
	title    := strings.TrimSpace(r.FormValue("title"))
	body     := r.FormValue("body")
	status   := strings.TrimSpace(r.FormValue("status"))
	coverURL := strings.TrimSpace(r.FormValue("cover_url"))

	if title == "" {
		title = "Untitled"
	}
	if status != "published" {
		status = "draft"
	}

	// Render Markdown once server-side.
	bodyHTML := string(renderMarkdown(body))
	excerpt  := articleExcerpt(body)

	if id == "" {
		// New article.
		slug := h.uniqueSlug(slugify(title), "")
		var pubAt interface{}
		if status == "published" {
			pubAt = time.Now()
		}
		var newID string
		err := h.db.QueryRow(`
			INSERT INTO articles (author_id, slug, title, body, body_html, excerpt, cover_url, status, published_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING id`,
			user.ID, slug, title, body, bodyHTML, excerpt, coverURL, status, pubAt,
		).Scan(&newID)
		if err != nil {
			log.Printf("[article] insert error: %v", err)
			http.Error(w, "Could not save article.", http.StatusInternalServerError)
			return
		}
		go func() {
			if verr := dbpkg.SaveArticleVersion(h.db, newID, user.ID, title, body, bodyHTML, excerpt, coverURL, "initial"); verr != nil {
				log.Printf("[article] version save error: %v", verr)
			}
		}()
		if status == "published" {
			http.Redirect(w, r, "/article/"+slug, http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/articles/"+newID+"/edit", http.StatusSeeOther)
		}
		return
	}

	// Update existing — verify ownership first.
	existing := h.getArticle(id)
	if existing == nil || existing.AuthorID != user.ID {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	// Only regenerate slug if title changed significantly.
	slug := existing.Slug
	if slugify(title) != slugify(existing.Title) {
		slug = h.uniqueSlug(slugify(title), id)
	}

	var pubAt interface{}
	if status == "published" {
		if existing.PublishedAt != nil {
			pubAt = existing.PublishedAt
		} else {
			pubAt = time.Now()
		}
	}

	_, err := h.db.Exec(`
		UPDATE articles
		SET slug = $1, title = $2, body = $3, body_html = $4, excerpt = $5,
		    cover_url = $6, status = $7, published_at = $8, updated_at = NOW()
		WHERE id = $9 AND author_id = $10`,
		slug, title, body, bodyHTML, excerpt, coverURL, status, pubAt, id, user.ID)
	if err != nil {
		log.Printf("[article] update error: %v", err)
		http.Error(w, "Could not save article.", http.StatusInternalServerError)
		return
	}
	go func() {
		if verr := dbpkg.SaveArticleVersion(h.db, id, user.ID, title, body, bodyHTML, excerpt, coverURL, "edit"); verr != nil {
			log.Printf("[article] version save error: %v", verr)
		}
	}()

	if status == "published" {
		http.Redirect(w, r, "/article/"+slug, http.StatusSeeOther)
	} else {
		http.Redirect(w, r, "/articles/"+id+"/edit", http.StatusSeeOther)
	}
}

// deleteArticle archives an article (soft delete).
// POST /api/article/{id}/delete
func (h *Handler) deleteArticle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "405", http.StatusMethodNotAllowed)
		return
	}
	user := h.userFromRequest(w, r)
	id := r.PathValue("id")
	_, err := h.db.Exec(
		`UPDATE articles SET status = 'archived', updated_at = NOW() WHERE id = $1 AND author_id = $2`,
		id, user.ID)
	if err != nil {
		http.Error(w, "Could not delete article.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/articles", http.StatusSeeOther)
}

// previewArticle renders a Markdown body as HTML for the editor preview tab.
// POST /api/article/preview  field: body
func (h *Handler) previewArticle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	body := r.FormValue("body")
	rendered := renderMarkdown(body)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Wrap in article prose class so preview CSS applies.
	fmt.Fprintf(w, `<div class="article-body">%s</div>`, template.HTML(rendered))
}

// facetArticleViewcount returns the live view count span for HTMX refresh.
// GET /facets/article/{id}/viewcount
func (h *Handler) facetArticleViewcount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var count int64
	if id != "" && h.db != nil {
		h.db.QueryRow(`SELECT view_count FROM articles WHERE id = $1`, id).Scan(&count)
	}
	hxAttrs := fmt.Sprintf(`id="article-views-%s" hx-get="/facets/article/%s/viewcount" hx-trigger="every 60s" hx-swap="outerHTML"`, id, id)
	if count > 0 {
		fmt.Fprintf(w, `<span %s><span class="article-byline-views">%d views</span></span>`, hxAttrs, count)
	} else {
		fmt.Fprintf(w, `<span %s></span>`, hxAttrs)
	}
}
