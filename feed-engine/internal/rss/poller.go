// Package rss provides a background RSS/Atom feed poller with in-memory cache.
// Feeds are fetched concurrently every pollInterval. Results are stored by category
// and served to the right-rail renderer via Cache.Get.
package rss

import (
	"context"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Item is a normalized feed entry from any RSS or Atom source.
type Item struct {
	Title     string
	URL       string
	Source    string
	ImageURL  string
	PublishedAt time.Time
	Category  string
}

type feedDef struct {
	URL      string
	Source   string
	Category string
}

// feeds is the master list. Categories: news | tech | music | crypto | sports | finance | forex
var feeds = []feedDef{
	// News
	{"https://feeds.bbci.co.uk/news/world/rss.xml", "BBC News", "news"},
	{"https://feeds.theguardian.com/theguardian/world/rss", "The Guardian", "news"},
	{"https://feeds.npr.org/1001/rss.xml", "NPR", "news"},
	{"https://www.aljazeera.com/xml/rss/all.xml", "Al Jazeera", "news"},
	{"https://feeds.apnews.com/rss/apf-topnews", "AP News", "news"},
	// Tech
	{"https://techcrunch.com/feed/", "TechCrunch", "tech"},
	{"https://feeds.arstechnica.com/arstechnica/index", "Ars Technica", "tech"},
	{"https://news.ycombinator.com/rss", "Hacker News", "tech"},
	// Music
	{"https://www.billboard.com/feed/", "Billboard", "music"},
	{"https://pitchfork.com/rss/news/", "Pitchfork", "music"},
	{"https://stereogum.com/feed/", "Stereogum", "music"},
	{"https://nme.com/feed", "NME", "music"},
	// Crypto
	{"https://www.coindesk.com/arc/outboundfeeds/rss/", "CoinDesk", "crypto"},
	{"https://cointelegraph.com/rss", "Cointelegraph", "crypto"},
	{"https://decrypt.co/feed", "Decrypt", "crypto"},
	// Sports
	{"https://www.espn.com/espn/rss/news", "ESPN", "sports"},
	{"https://sports.yahoo.com/rss/", "Yahoo Sports", "sports"},
	{"https://www.cbssports.com/rss/headlines/", "CBS Sports", "sports"},
	// Finance
	{"https://www.cnbc.com/id/100003114/device/rss/rss.html", "CNBC", "finance"},
	{"https://feeds.marketwatch.com/marketwatch/topstories/", "MarketWatch", "finance"},
	{"https://seekingalpha.com/market_currents.xml", "Seeking Alpha", "finance"},
	// Forex
	{"https://www.forexlive.com/feed/news", "ForexLive", "forex"},
	{"https://www.fxstreet.com/rss/news", "FXStreet", "forex"},
}

// Cache holds the latest items per category, refreshed on a background ticker.
type Cache struct {
	mu     sync.RWMutex
	items  map[string][]Item
	client *http.Client
}

func NewCache() *Cache {
	return &Cache{
		items:  make(map[string][]Item),
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

// Get returns up to limit items for the given category, sorted newest-first.
func (c *Cache) Get(category string, limit int) []Item {
	c.mu.RLock()
	items := c.items[category]
	c.mu.RUnlock()
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

// Start launches the background polling goroutine.
func (c *Cache) Start(ctx context.Context, interval time.Duration) {
	go func() {
		c.refresh()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.refresh()
			}
		}
	}()
}

func (c *Cache) refresh() {
	type result struct {
		category string
		items    []Item
	}
	ch := make(chan result, len(feeds))
	for _, f := range feeds {
		f := f
		go func() {
			items, err := c.fetch(f)
			if err != nil {
				log.Printf("[rss] %s fetch failed: %v", f.Source, err)
				ch <- result{f.Category, nil}
				return
			}
			ch <- result{f.Category, items}
		}()
	}

	byCategory := make(map[string][]Item)
	for range feeds {
		r := <-ch
		byCategory[r.category] = append(byCategory[r.category], r.items...)
	}

	c.mu.Lock()
	for cat, items := range byCategory {
		sort.Slice(items, func(i, j int) bool {
			return items[i].PublishedAt.After(items[j].PublishedAt)
		})
		if len(items) > 20 {
			items = items[:20]
		}
		c.items[cat] = items
	}
	c.mu.Unlock()
}

// ── XML structs for RSS 2.0 ────────────────────────────────────────────────────

type rss2Doc struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rss2Channel `xml:"channel"`
}

type rss2Channel struct {
	Items []rss2Item `xml:"item"`
}

type rss2Item struct {
	Title     string `xml:"title"`
	Link      string `xml:"link"`
	PubDate   string `xml:"pubDate"`
	Enclosure struct {
		URL  string `xml:"url,attr"`
		Type string `xml:"type,attr"`
	} `xml:"enclosure"`
}

// ── XML structs for Atom ───────────────────────────────────────────────────────

type atomDoc struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Links     []atomLink `xml:"link"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

// ── fetch ─────────────────────────────────────────────────────────────────────

func (c *Cache) fetch(f feedDef) ([]Item, error) {
	req, err := http.NewRequest(http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "F33D3R/1.0 RSS Reader (+https://f33d3r.com)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, text/xml, */*")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, err
	}

	// Try RSS 2.0
	var rss rss2Doc
	if err := xml.Unmarshal(body, &rss); err == nil && len(rss.Channel.Items) > 0 {
		return rss2Items(rss, f), nil
	}

	// Try Atom
	var atom atomDoc
	if err := xml.Unmarshal(body, &atom); err == nil && len(atom.Entries) > 0 {
		return atomItems(atom, f), nil
	}

	return nil, nil
}

func rss2Items(doc rss2Doc, f feedDef) []Item {
	out := make([]Item, 0, len(doc.Channel.Items))
	for _, it := range doc.Channel.Items {
		if it.Title == "" || it.Link == "" {
			continue
		}
		item := Item{
			Title:    cleanTitle(it.Title),
			URL:      it.Link,
			Source:   f.Source,
			Category: f.Category,
		}
		if it.Enclosure.URL != "" && isImageType(it.Enclosure.Type) {
			item.ImageURL = it.Enclosure.URL
		}
		item.PublishedAt = parseDate(it.PubDate)
		out = append(out, item)
	}
	return out
}

func atomItems(doc atomDoc, f feedDef) []Item {
	out := make([]Item, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		if e.Title == "" {
			continue
		}
		link := ""
		for _, l := range e.Links {
			if l.Rel == "alternate" || l.Rel == "" {
				link = l.Href
				break
			}
		}
		if link == "" {
			continue
		}
		pub := e.Published
		if pub == "" {
			pub = e.Updated
		}
		out = append(out, Item{
			Title:       cleanTitle(e.Title),
			URL:         link,
			Source:      f.Source,
			Category:    f.Category,
			PublishedAt: parseDate(pub),
		})
	}
	return out
}

// ── helpers ───────────────────────────────────────────────────────────────────

var dateFmts = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC3339,
	"2006-01-02T15:04:05Z",
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
}

func parseDate(s string) time.Time {
	for _, f := range dateFmts {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Now()
}

func isImageType(mime string) bool {
	return len(mime) >= 5 && mime[:5] == "image"
}

func cleanTitle(s string) string {
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
