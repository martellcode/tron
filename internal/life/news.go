package life

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NewsReader fetches and filters news from various sources.
type NewsReader struct {
	baseDir string
	client  *http.Client

	mu       sync.RWMutex
	saved    []Article
	lastRead map[string]time.Time // URL -> when read
}

// Article represents a news article.
type Article struct {
	ID        int       `json:"id,omitempty"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Source    string    `json:"source"`
	Score     int       `json:"score,omitempty"`
	Comments  int       `json:"comments,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	Tags      []string  `json:"tags,omitempty"`
}

// PersonaSources maps each persona to their news sources
var PersonaSources = map[string][]NewsSource{
	"Tony": {
		{Name: "hackernews", Type: "hackernews"},
		{Name: "lobsters", Type: "lobsters"},
	},
	"Maya": {
		{Name: "marketing", Type: "reddit", Subreddit: "marketing"},
		{Name: "socialmedia", Type: "reddit", Subreddit: "socialmedia"},
		{Name: "adops", Type: "reddit", Subreddit: "adops"},
	},
	"Alex": {
		{Name: "startups", Type: "reddit", Subreddit: "startups"},
		{Name: "entrepreneur", Type: "reddit", Subreddit: "Entrepreneur"},
		{Name: "business", Type: "reddit", Subreddit: "business"},
	},
	"Jordan": {
		{Name: "finance", Type: "reddit", Subreddit: "finance"},
		{Name: "venturecapital", Type: "reddit", Subreddit: "venturecapital"},
		{Name: "startups", Type: "reddit", Subreddit: "startups"}, // for funding news
	},
	"Riley": {
		{Name: "producthunt", Type: "producthunt"},
		{Name: "productmanagement", Type: "reddit", Subreddit: "ProductManagement"},
		{Name: "userexperience", Type: "reddit", Subreddit: "userexperience"},
	},
}

// NewsSource represents a news source configuration
type NewsSource struct {
	Name      string
	Type      string // "hackernews", "lobsters", "reddit", "producthunt", "rss"
	Subreddit string // for reddit sources
	URL       string // for rss sources
}

// HNResponse represents a Hacker News API response.
type HNResponse struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"`
	Type        string `json:"type"`
}

// LobstersResponse represents a Lobste.rs story
type LobstersResponse struct {
	ShortID      string   `json:"short_id"`
	Title        string   `json:"title"`
	URL          string   `json:"url"`
	Score        int      `json:"score"`
	CommentCount int      `json:"comment_count"`
	Tags         []string `json:"tags"`
}

// RedditResponse represents Reddit's JSON API response
type RedditResponse struct {
	Data struct {
		Children []struct {
			Data struct {
				Title     string  `json:"title"`
				URL       string  `json:"url"`
				Permalink string  `json:"permalink"`
				Score     int     `json:"score"`
				NumComments int   `json:"num_comments"`
				Subreddit string  `json:"subreddit"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// ProductHuntPost represents a Product Hunt post from RSS
type ProductHuntPost struct {
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

// NewNewsReader creates a new news reader.
func NewNewsReader(baseDir string) *NewsReader {
	nr := &NewsReader{
		baseDir:  baseDir,
		client:   &http.Client{Timeout: 30 * time.Second},
		saved:    make([]Article, 0),
		lastRead: make(map[string]time.Time),
	}
	nr.load()
	return nr
}

// FetchForPersona retrieves news from sources specific to the given persona.
func (n *NewsReader) FetchForPersona(ctx context.Context, persona string) ([]Article, error) {
	sources, ok := PersonaSources[persona]
	if !ok {
		// Default to HN for unknown personas
		sources = PersonaSources["Tony"]
	}

	var allArticles []Article
	for _, source := range sources {
		var articles []Article
		var err error

		switch source.Type {
		case "hackernews":
			articles, err = n.fetchHackerNews(ctx)
		case "lobsters":
			articles, err = n.fetchLobsters(ctx)
		case "reddit":
			articles, err = n.fetchReddit(ctx, source.Subreddit)
		case "producthunt":
			articles, err = n.fetchProductHunt(ctx)
		}

		if err != nil {
			fmt.Printf("[news] %s fetch error for %s: %v\n", persona, source.Name, err)
			continue
		}

		allArticles = append(allArticles, articles...)
	}

	return allArticles, nil
}

// Fetch retrieves news from Hacker News (legacy method for backwards compatibility).
func (n *NewsReader) Fetch(ctx context.Context) ([]Article, error) {
	return n.FetchForPersona(ctx, "Tony")
}

// fetchHackerNews gets top stories from HN.
func (n *NewsReader) fetchHackerNews(ctx context.Context) ([]Article, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://hacker-news.firebaseio.com/v0/topstories.json", nil)
	if err != nil {
		return nil, err
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var storyIDs []int
	if err := json.NewDecoder(resp.Body).Decode(&storyIDs); err != nil {
		return nil, err
	}

	// Fetch top 20 stories
	limit := 20
	if len(storyIDs) < limit {
		limit = len(storyIDs)
	}

	var articles []Article
	for _, id := range storyIDs[:limit] {
		article, err := n.fetchHNStory(ctx, id)
		if err != nil {
			continue
		}
		if article != nil {
			articles = append(articles, *article)
		}
	}

	return articles, nil
}

// fetchHNStory fetches a single HN story.
func (n *NewsReader) fetchHNStory(ctx context.Context, id int) (*Article, error) {
	url := fmt.Sprintf("https://hacker-news.firebaseio.com/v0/item/%d.json", id)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var story HNResponse
	if err := json.NewDecoder(resp.Body).Decode(&story); err != nil {
		return nil, err
	}

	if story.Type != "story" || story.URL == "" {
		return nil, nil
	}

	return &Article{
		ID:        story.ID,
		Title:     story.Title,
		URL:       story.URL,
		Source:    "hackernews",
		Score:     story.Score,
		Comments:  story.Descendants,
		FetchedAt: time.Now(),
	}, nil
}

// fetchLobsters gets stories from Lobste.rs
func (n *NewsReader) fetchLobsters(ctx context.Context) ([]Article, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://lobste.rs/hottest.json", nil)
	if err != nil {
		return nil, err
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var stories []LobstersResponse
	if err := json.NewDecoder(resp.Body).Decode(&stories); err != nil {
		return nil, err
	}

	var articles []Article
	limit := 15
	if len(stories) < limit {
		limit = len(stories)
	}

	for _, story := range stories[:limit] {
		if story.URL == "" {
			continue
		}
		articles = append(articles, Article{
			Title:     story.Title,
			URL:       story.URL,
			Source:    "lobsters",
			Score:     story.Score,
			Comments:  story.CommentCount,
			Tags:      story.Tags,
			FetchedAt: time.Now(),
		})
	}

	return articles, nil
}

// fetchReddit gets top posts from a subreddit
func (n *NewsReader) fetchReddit(ctx context.Context, subreddit string) ([]Article, error) {
	url := fmt.Sprintf("https://www.reddit.com/r/%s/hot.json?limit=15", subreddit)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Reddit requires a user agent
	req.Header.Set("User-Agent", "TronNewsReader/1.0")

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("reddit returned status %d", resp.StatusCode)
	}

	var redditResp RedditResponse
	if err := json.NewDecoder(resp.Body).Decode(&redditResp); err != nil {
		return nil, err
	}

	var articles []Article
	for _, child := range redditResp.Data.Children {
		post := child.Data
		// Skip self posts (discussions without external links)
		articleURL := post.URL
		if strings.HasPrefix(articleURL, "/r/") || strings.HasPrefix(articleURL, "https://www.reddit.com") {
			// It's a self post or reddit link, use the permalink instead
			articleURL = "https://www.reddit.com" + post.Permalink
		}

		articles = append(articles, Article{
			Title:     post.Title,
			URL:       articleURL,
			Source:    "reddit/" + post.Subreddit,
			Score:     post.Score,
			Comments:  post.NumComments,
			FetchedAt: time.Now(),
		})
	}

	return articles, nil
}

// fetchProductHunt gets recent products from Product Hunt RSS
func (n *NewsReader) fetchProductHunt(ctx context.Context) ([]Article, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.producthunt.com/feed", nil)
	if err != nil {
		return nil, err
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse RSS
	type RSSItem struct {
		Title string `xml:"title"`
		Link  string `xml:"link"`
	}
	type RSSChannel struct {
		Items []RSSItem `xml:"item"`
	}
	type RSS struct {
		Channel RSSChannel `xml:"channel"`
	}

	var rss RSS
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, err
	}

	var articles []Article
	limit := 10
	if len(rss.Channel.Items) < limit {
		limit = len(rss.Channel.Items)
	}

	for _, item := range rss.Channel.Items[:limit] {
		articles = append(articles, Article{
			Title:     item.Title,
			URL:       item.Link,
			Source:    "producthunt",
			FetchedAt: time.Now(),
		})
	}

	return articles, nil
}

// PersonaTopics maps each persona's focus areas to expanded keyword lists
var PersonaTopics = map[string][]string{
	// Tony (CTO) - Technical depth
	"engineering":    {"engineer", "engineering", "code", "coding", "developer", "programming", "software"},
	"technology":     {"tech", "technology", "technical"},
	"architecture":   {"architecture", "system design", "distributed", "microservice", "monolith"},
	"ai":             {"ai", "llm", "gpt", "claude", "anthropic", "openai", "machine learning", "ml", "neural"},
	"infrastructure": {"infrastructure", "devops", "kubernetes", "docker", "aws", "cloud", "terraform", "deploy"},

	// Maya (CMO) - Marketing & Growth
	"marketing":   {"marketing", "brand", "branding", "ads", "advertising", "campaign", "seo", "content"},
	"growth":      {"growth", "viral", "acquisition", "retention", "churn", "funnel", "conversion"},
	"brand":       {"brand", "rebrand", "positioning", "messaging", "storytelling"},
	"customers":   {"customer", "user research", "feedback", "nps", "satisfaction", "audience"},
	"positioning": {"positioning", "differentiation", "competitive", "market fit"},

	// Alex (CEO) - Strategy & Leadership
	"strategy":   {"strategy", "strategic", "pivot", "direction", "roadmap", "vision"},
	"leadership": {"leadership", "leader", "ceo", "founder", "executive", "management", "hiring"},
	"vision":     {"vision", "mission", "future", "long-term", "scale"},
	"culture":    {"culture", "values", "remote", "team building", "morale"},
	"execution":  {"execution", "okr", "kpi", "metrics", "performance", "goals"},

	// Jordan (CFO) - Finance & Operations
	"finance":        {"finance", "financial", "revenue", "profit", "margin", "cash", "accounting"},
	"runway":         {"runway", "burn", "burn rate", "capital", "cashflow"},
	"unit economics": {"unit economics", "ltv", "cac", "arpu", "margin", "roi"},
	"fundraising":    {"fundraising", "funding", "series", "valuation", "investor", "vc", "venture", "raise"},
	"budgets":        {"budget", "cost", "spending", "expense", "forecast"},

	// Riley (CPO) - Product & Users
	"product":  {"product", "feature", "ship", "launch", "release", "pm", "product manager", "roadmap"},
	"users":    {"user", "ux", "usability", "experience", "interface", "ui", "customer"},
	"roadmap":  {"roadmap", "backlog", "priorit", "sprint", "agile"},
	"ux":       {"ux", "design", "usability", "accessibility", "a11y", "prototype"},
	"research": {"research", "discovery", "interview", "survey", "data", "insight"},
}

// Topics to always avoid (shared across all personas)
var avoidTopics = []string{
	"crypto", "bitcoin", "nft", "blockchain", "web3",
	"lawsuit", "fired", "layoff",
	"politics", "election",
}

// Filter returns articles that would be interesting (legacy method).
func (n *NewsReader) Filter(articles []Article) []Article {
	return n.FilterForPersona(articles, []string{"ai", "engineering", "startup"})
}

// FilterForPersona returns articles scored and filtered by persona's focus areas.
func (n *NewsReader) FilterForPersona(articles []Article, focusAreas []string) []Article {
	type scoredArticle struct {
		article Article
		score   float64
	}

	var scored []scoredArticle

	for _, article := range articles {
		titleLower := strings.ToLower(article.Title)

		// Skip if already read
		n.mu.RLock()
		_, alreadyRead := n.lastRead[article.URL]
		n.mu.RUnlock()
		if alreadyRead {
			continue
		}

		// Skip avoided topics
		skip := false
		for _, avoid := range avoidTopics {
			if strings.Contains(titleLower, avoid) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Score article based on persona's focus areas
		var relevanceScore float64
		for _, focus := range focusAreas {
			keywords, ok := PersonaTopics[focus]
			if !ok {
				// Direct match on focus area itself
				if strings.Contains(titleLower, strings.ToLower(focus)) {
					relevanceScore += 10
				}
				continue
			}
			for _, keyword := range keywords {
				if strings.Contains(titleLower, keyword) {
					relevanceScore += 10
					break // Only count each focus area once
				}
			}
		}

		// Bonus for high scores on the source
		if article.Score > 300 {
			relevanceScore += 5
		} else if article.Score > 100 {
			relevanceScore += 3
		} else if article.Score > 50 {
			relevanceScore += 1
		}

		// Only include if there's some relevance
		if relevanceScore > 0 {
			scored = append(scored, scoredArticle{article: article, score: relevanceScore})
		}
	}

	// Sort by score (highest first)
	for i := 0; i < len(scored); i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[i].score {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}

	// Return top 3
	var interesting []Article
	limit := 3
	if len(scored) < limit {
		limit = len(scored)
	}
	for i := 0; i < limit; i++ {
		interesting = append(interesting, scored[i].article)
	}

	return interesting
}

// Save stores an article in the reading list.
func (n *NewsReader) Save(article Article) {
	n.mu.Lock()
	defer n.mu.Unlock()

	article.FetchedAt = time.Now()
	n.saved = append(n.saved, article)
	n.lastRead[article.URL] = time.Now()

	// Keep only last 100 saved articles
	if len(n.saved) > 100 {
		n.saved = n.saved[len(n.saved)-100:]
	}

	n.persist()
}

// RecentSaved returns recently saved articles.
func (n *NewsReader) RecentSaved(within time.Duration) []Article {
	n.mu.RLock()
	defer n.mu.RUnlock()

	cutoff := time.Now().Add(-within)
	var recent []Article
	for _, a := range n.saved {
		if a.FetchedAt.After(cutoff) {
			recent = append(recent, a)
		}
	}
	return recent
}

// persist saves state to disk.
func (n *NewsReader) persist() {
	dir := filepath.Join(n.baseDir, "life")
	os.MkdirAll(dir, 0755)

	data := struct {
		Saved    []Article            `json:"saved"`
		LastRead map[string]time.Time `json:"last_read"`
	}{
		Saved:    n.saved,
		LastRead: n.lastRead,
	}

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}

	os.WriteFile(filepath.Join(dir, "news.json"), jsonData, 0644)
}

// load restores state from disk.
func (n *NewsReader) load() {
	path := filepath.Join(n.baseDir, "life", "news.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var state struct {
		Saved    []Article            `json:"saved"`
		LastRead map[string]time.Time `json:"last_read"`
	}

	if err := json.Unmarshal(data, &state); err != nil {
		return
	}

	n.saved = state.Saved
	n.lastRead = state.LastRead
}
