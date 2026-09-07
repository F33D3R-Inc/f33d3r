package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// translatePostHandler translates a post body to English, caching the result
// in the posts table so repeat requests are instant.
// POST /api/post/{id}/translate
// Returns an HTML fragment (translate_result Facet) for HTMX swap into
// #translate-wrap-{id}.
func (h *Handler) translatePostHandler(w http.ResponseWriter, r *http.Request) {
	postID := r.PathValue("id")
	if postID == "" {
		h.renderTranslateError(w, postID, "Invalid post.")
		return
	}

	if h.db == nil {
		h.renderTranslateError(w, postID, "Database unavailable.")
		return
	}

	var body, cachedBody, cachedLang string
	err := h.db.QueryRow(`
		SELECT body,
		       COALESCE(translated_body, ''),
		       COALESCE(translated_lang, '')
		FROM works
		WHERE id = $1 AND deleted_at IS NULL`, postID).
		Scan(&body, &cachedBody, &cachedLang)
	if err != nil {
		h.renderTranslateError(w, postID, "Post not found.")
		return
	}

	// Serve cached translation if available.
	if cachedBody != "" {
		h.renderTranslateResult(w, postID, cachedBody, cachedLang)
		return
	}

	if h.cfg.TranslationAPIURL == "" {
		h.renderTranslateError(w, postID, "Translation not configured on this server.")
		return
	}

	translated, lang, err := h.callTranslationAPI(body)
	if err != nil {
		log.Printf("[translate] API error for post %s: %v", postID, err)
		h.renderTranslateError(w, postID, "Translation service unavailable.")
		return
	}

	// Cache result — fire and forget, don't block the response.
	go func() {
		_, dbErr := h.db.Exec(`
			UPDATE works SET translated_body = $1, translated_lang = $2 WHERE id = $3`,
			translated, lang, postID)
		if dbErr != nil {
			log.Printf("[translate] cache write error: %v", dbErr)
		}
	}()

	h.renderTranslateResult(w, postID, translated, lang)
}

func (h *Handler) callTranslationAPI(text string) (translated, lang string, err error) {
	if len(text) > 5000 {
		text = text[:5000]
	}
	payload := map[string]string{
		"q":      text,
		"source": "auto",
		"target": "en",
		"format": "text",
	}
	if h.cfg.TranslationAPIKey != "" {
		payload["api_key"] = h.cfg.TranslationAPIKey
	}
	b, _ := json.Marshal(payload)

	resp, err := h.httpClient.Post(
		h.cfg.TranslationAPIURL+"/translate",
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		return "", "", fmt.Errorf("translation request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		TranslatedText   string `json:"translatedText"`
		DetectedLanguage struct {
			Language   string  `json:"language"`
			Confidence float64 `json:"confidence"`
		} `json:"detectedLanguage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("translation decode: %w", err)
	}
	if result.TranslatedText == "" {
		return "", "", fmt.Errorf("empty translation response")
	}
	return result.TranslatedText, result.DetectedLanguage.Language, nil
}

func (h *Handler) renderTranslateResult(w http.ResponseWriter, postID, translated, lang string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "translate_result", map[string]interface{}{
		"PostID":         postID,
		"TranslatedText": translated,
		"DetectedLang":   lang,
	})
}

func (h *Handler) renderTranslateError(w http.ResponseWriter, postID, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "translate_error", map[string]interface{}{
		"PostID": postID,
		"Reason": reason,
	})
}
