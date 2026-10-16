package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Web lookups for the harness. DuckDuckGo's HTML page sometimes answers a program with a bot check and no
// results; a model that is told "No results" then invents an answer. So search tries several sources in
// turn, and when none answers it says plainly that the lookup failed. Weather, which is a pure lookup,
// is answered by the harness directly.

func (a *app) fetchURL(ctx context.Context, address string, limit int64) (string, error) {
	return a.fetchURLAs(ctx, "", address, limit)
}

func (a *app) fetchURLAs(ctx context.Context, userAgent, address string, limit int64) (string, error) {
	if a.harness.fetch != nil {
		return a.harness.fetch(ctx, address)
	}
	var body string
	var err error
	if userAgent == "" {
		body, _, err = fetchPublic(ctx, address, limit)
	} else {
		body, _, err = fetchPublicAs(ctx, address, userAgent, limit)
	}
	return body, err
}

var (
	ddgLiteLinkRE    = regexp.MustCompile(`(?s)<a[^>]+class=['"]result-link['"][^>]*href=['"]([^'"]+)['"][^>]*>(.*?)</a>.*?<td[^>]+class=['"]result-snippet['"][^>]*>(.*?)</td>`)
	ddgLiteLinkRevRE = regexp.MustCompile(`(?s)<a[^>]+href=['"]([^'"]+)['"][^>]+class=['"]result-link['"][^>]*>(.*?)</a>.*?<td[^>]+class=['"]result-snippet['"][^>]*>(.*?)</td>`)
)

func formatResult(n int, title, link, snippet string) string {
	return fmt.Sprintf("%d. %s\n   %s\n   %s\n", n, strings.TrimSpace(title), link, strings.TrimSpace(snippet))
}

func parseDDGResults(body string, lite bool) string {
	var b strings.Builder
	n := 0
	matches := ddgResultRE.FindAllStringSubmatch(body, 8)
	if lite {
		matches = ddgLiteLinkRE.FindAllStringSubmatch(body, 8)
		if len(matches) == 0 {
			matches = ddgLiteLinkRevRE.FindAllStringSubmatch(body, 8)
		}
	}
	for _, m := range matches {
		link := html.UnescapeString(m[1])
		if r := ddgRedirectR.FindStringSubmatch(link); r != nil {
			if decoded, err := url.QueryUnescape(r[1]); err == nil {
				link = decoded
			}
		}
		n++
		b.WriteString(formatResult(n, htmlToText(m[2]), link, htmlToText(m[3])))
	}
	return b.String()
}

// wikipediaSearch is the fallback source: the top matching articles with their opening summaries.
func (a *app) wikipediaSearch(ctx context.Context, query string) string {
	body, err := a.fetchURL(ctx, "https://en.wikipedia.org/w/api.php?action=query&list=search&format=json&srlimit=3&srsearch="+url.QueryEscape(query), 1<<20)
	if err != nil {
		return ""
	}
	var found struct {
		Query struct {
			Search []struct {
				Title string `json:"title"`
			} `json:"search"`
		} `json:"query"`
	}
	if json.Unmarshal([]byte(body), &found) != nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, hit := range found.Query.Search {
		if n >= 2 {
			break
		}
		page := strings.ReplaceAll(hit.Title, " ", "_")
		summary, err := a.fetchURL(ctx, "https://en.wikipedia.org/api/rest_v1/page/summary/"+url.PathEscape(page), 1<<20)
		if err != nil {
			continue
		}
		var s struct {
			Extract string `json:"extract"`
		}
		if json.Unmarshal([]byte(summary), &s) != nil || strings.TrimSpace(s.Extract) == "" {
			continue
		}
		n++
		b.WriteString(formatResult(n, hit.Title+" (Wikipedia)", "https://en.wikipedia.org/wiki/"+page, clipText(s.Extract, 600)))
	}
	return b.String()
}

// webSearch looks something up, trying each source in turn. An error means every source failed.
func (a *app) webSearch(ctx context.Context, query string) (string, error) {
	if out := parseDDGResults(a.searchPage(ctx, "https://html.duckduckgo.com/html/", query), false); out != "" {
		return out, nil
	}
	if out := parseDDGResults(a.searchPage(ctx, "https://lite.duckduckgo.com/lite/", query), true); out != "" {
		return out, nil
	}
	// An encyclopedia is a fair fallback for a timeless question, but for "the current ..." or "the latest ..."
	// it gives a confident, out-of-date or beside-the-point answer, which is worse than saying so.
	if !webLookupRE.MatchString(query) {
		if out := a.wikipediaSearch(ctx, query); out != "" {
			return out, nil
		}
	}
	return "", errors.New("the web search did not answer (the search sites are not responding or are blocking it). Tell the user you could not look that up right now. Do not guess")
}

// searchPage fetches a DuckDuckGo results page. DuckDuckGo answers a plain GET from a program with a bot
// check, but serves the same form submitted the way a browser submits it, so the real request is a POST.
func (a *app) searchPage(ctx context.Context, endpoint, query string) string {
	if a.harness.fetch != nil { // tests
		body, err := a.harness.fetch(ctx, endpoint+"?q="+url.QueryEscape(query))
		if err != nil {
			return ""
		}
		return body
	}
	body, err := postPublic(ctx, endpoint, url.Values{"q": {query}, "b": {""}, "kl": {"wt-wt"}})
	if err != nil {
		return ""
	}
	return body
}

// postPublic submits a form to a public web address (never to this computer or the local network).
func postPublic(ctx context.Context, address string, form url.Values) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Referer", address)
	response, err := publicOnlyClient().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return "", fmt.Errorf("the site answered %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return string(data), err
}

// --- weather ------------------------------------------------------------------

var (
	weatherRE      = regexp.MustCompile(`(?i)\b(weather|temperature|how (?:hot|cold|warm)|is it (?:raining|snowing|sunny|cold|hot|warm))\b`)
	weatherLaterRE = regexp.MustCompile(`(?i)\b(tomorrow|week|weekend|next|forecast|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)
	weatherPlaceRE = regexp.MustCompile(`(?i)\b(?:in|for|at|near|of)\s+([A-Za-z][A-Za-z.' -]{1,40}?)(?:\s+(?:today|now|right now|tonight|at the moment|this (?:morning|afternoon|evening)|like))*\s*[?.!]*\s*$`)
)

// weatherNow answers "what's the weather in London?" exactly, from wttr.in. It declines (ok false) when no
// place is named or the question is about later than now.
func (a *app) weatherNow(ctx context.Context, text string) (string, bool) {
	if !weatherRE.MatchString(text) || weatherLaterRE.MatchString(text) {
		return "", false
	}
	m := weatherPlaceRE.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	place := strings.TrimSpace(m[1])
	switch strings.ToLower(place) {
	case "", "the", "my area", "here", "today", "now":
		return "", false
	}
	body, err := a.fetchURLAs(ctx, "curl/8.5.0", "https://wttr.in/"+url.PathEscape(place)+"?format="+url.QueryEscape("%C, %t (feels %f), wind %w, humidity %h"), 1<<16)
	body = strings.TrimSpace(body)
	if err != nil || body == "" || strings.Contains(strings.ToLower(body), "unknown location") || strings.Contains(body, "<") || len(body) > 200 {
		return "", false
	}
	body = strings.NewReplacer("←", "", "↑", "", "→", "", "↓", "", "↖", "", "↗", "", "↘", "", "↙", "").Replace(body) // wind arrows read badly aloud
	body = regexp.MustCompile(`\+(\d)`).ReplaceAllString(body, "$1")
	return fmt.Sprintf("In %s right now: %s.", place, strings.TrimRight(strings.TrimSpace(body), ". ")), true
}
