// crawler discovers and traverses all reachable routes.
// It parses HTML responses for hrefs, HTMX targets, and form actions,
// building a navigation graph and de-duplicating visited paths.
package crawler

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Node struct {
	URL      string
	Status   int
	Latency  time.Duration
	Links    []string
	FromURL  string
	Depth    int
	Identity string
}

type Graph struct {
	mu      sync.Mutex
	visited map[string]bool
	Nodes   []*Node
}

func NewGraph() *Graph {
	return &Graph{visited: make(map[string]bool)}
}

func (g *Graph) seen(u string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.visited[u] { return true }
	g.visited[u] = true
	return false
}

func (g *Graph) add(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Nodes = append(g.Nodes, n)
}

// Crawl performs BFS traversal from seed URLs.
// maxDepth limits recursion. concurrency controls goroutines.
func Crawl(client *http.Client, baseURL string, seeds []string, identity string, maxDepth, concurrency int) *Graph {
	g := NewGraph()
	type work struct {
		url   string
		from  string
		depth int
	}

	queue := make(chan work, 1000)
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range queue {
				if w.depth > maxDepth { continue }
				if g.seen(w.url) { continue }

				node := fetch(client, w.url, w.from, w.depth, identity, baseURL)
				g.add(node)

				if w.depth < maxDepth {
					for _, link := range node.Links {
						queue <- work{url: link, from: w.url, depth: w.depth + 1}
					}
				}
			}
		}()
	}

	// Seed
	for _, s := range seeds {
		queue <- work{url: s, from: "seed", depth: 0}
	}

	// Give workers time then drain
	time.Sleep(200 * time.Millisecond)
	go func() {
		wg.Wait()
		close(queue)
	}()
	// Wait for drain
	time.Sleep(5 * time.Second)

	return g
}

func fetch(client *http.Client, rawURL, fromURL string, depth int, identity, baseURL string) *Node {
	n := &Node{
		URL:      rawURL,
		FromURL:  fromURL,
		Depth:    depth,
		Identity: identity,
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		n.Status = -1
		return n
	}
	if identity != "" {
		req.AddCookie(&http.Cookie{Name: "f33d3r_handle", Value: identity})
	}
	req.Header.Set("HX-Request", "false")
	req.Header.Set("User-Agent", "AethyrWalker/1.0")

	start := time.Now()
	resp, err := client.Do(req)
	n.Latency = time.Since(start)
	if err != nil {
		n.Status = -1
		return n
	}
	defer resp.Body.Close()

	n.Status = resp.StatusCode

	// Only parse HTML for links
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == 200 && strings.Contains(ct, "text/html") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		n.Links = extractLinks(string(body), rawURL, baseURL)
	}

	return n
}

func extractLinks(body, pageURL, baseURL string) []string {
	seen := map[string]bool{}
	var links []string

	doc, err := html.Parse(strings.NewReader(body))
	if err != nil { return links }

	base, _ := url.Parse(baseURL)
	page, _ := url.Parse(pageURL)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			attrs := attrMap(n)
			// Standard hrefs
			if href := attrs["href"]; href != "" && !strings.HasPrefix(href, "#") && !strings.HasPrefix(href, "javascript:") && !strings.HasPrefix(href, "mailto:") {
				if u := resolve(href, page, base); u != "" && !seen[u] {
					seen[u] = true
					links = append(links, u)
				}
			}
			// HTMX targets
			for _, attr := range []string{"hx-get", "hx-post"} {
				if v := attrs[attr]; v != "" {
					if u := resolve(v, page, base); u != "" && !seen[u] {
						seen[u] = true
						links = append(links, u)
					}
				}
			}
			// Form actions
			if action := attrs["action"]; action != "" {
				if u := resolve(action, page, base); u != "" && !seen[u] {
					seen[u] = true
					links = append(links, u)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return links
}

func resolve(href string, page, base *url.URL) string {
	ref, err := url.Parse(href)
	if err != nil { return "" }
	resolved := page.ResolveReference(ref)
	// Only keep same-origin links
	if resolved.Host != base.Host { return "" }
	// Strip query and fragment for dedup
	resolved.RawQuery = ""
	resolved.Fragment = ""
	s := resolved.String()
	// Skip static assets
	for _, ext := range []string{".css", ".js", ".png", ".jpg", ".gif", ".ico", ".svg", ".webp", ".woff", ".ttf"} {
		if strings.HasSuffix(s, ext) { return "" }
	}
	return s
}

func attrMap(n *html.Node) map[string]string {
	m := map[string]string{}
	for _, a := range n.Attr {
		m[a.Key] = a.Val
	}
	return m
}

// Summary returns a text coverage report.
func (g *Graph) Summary() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	total := len(g.Nodes)
	ok := 0; broken := 0; slow := 0; var slowPaths []string
	for _, n := range g.Nodes {
		if n.Status >= 200 && n.Status < 400 { ok++ } else { broken++ }
		if n.Latency > 2*time.Second { slow++; slowPaths = append(slowPaths, fmt.Sprintf("%s (%s)", n.URL, n.Latency.Round(time.Millisecond))) }
	}
	s := fmt.Sprintf("Paths: %d total | %d OK | %d broken | %d slow (>2s)\n", total, ok, broken, slow)
	for _, p := range slowPaths { s += "  🐢 " + p + "\n" }
	return s
}
