package life

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NewsReader fetches and filters tech news for Tony.
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

// HNResponse represents a Hacker News API response.
type HNResponse struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"` // comment count
	Type        string `json:"type"`
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

// Fetch retrieves news from various sources.
func (n *NewsReader) Fetch(ctx context.Context) ([]Article, error) {
	var allArticles []Article

	// Fetch from Hacker News
	hnArticles, err := n.fetchHackerNews(ctx)
	if err != nil {
		// Log but continue with other sources
		fmt.Printf("[news] HN fetch error: %v\n", err)
	} else {
		allArticles = append(allArticles, hnArticles...)
	}

	// TODO: Add more sources
	// - Lobste.rs
	// - Tech RSS feeds
	// - AI/ML newsletters

	return allArticles, nil
}

// fetchHackerNews gets top stories from HN.
func (n *NewsReader) fetchHackerNews(ctx context.Context) ([]Article, error) {
	// Get top story IDs
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

	// Fetch top 30 stories
	limit := 30
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

	// Skip non-stories and stories without URLs
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

// Filter returns articles that Tony would find interesting.
func (n *NewsReader) Filter(articles []Article) []Article {
	var interesting []Article

	// Topics Tony cares about
	relevantTopics := []string{
		"ai", "llm", "gpt", "claude", "anthropic", "openai",
		"golang", "go ", "rust", "typescript",
		"startup", "founder", "cto", "engineering",
		"distributed", "systems", "database", "postgres",
		"kubernetes", "docker", "cloud", "aws",
		"api", "microservice", "architecture",
		"security", "encryption", "auth",
		"open source", "oss",
	}

	// Topics to avoid
	avoidTopics := []string{
		"crypto", "bitcoin", "nft", "blockchain", "web3",
		"lawsuit", "fired", "layoff",
		"politics", "election",
	}

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

		// Check for relevance
		relevant := false
		for _, topic := range relevantTopics {
			if strings.Contains(titleLower, topic) {
				relevant = true
				break
			}
		}

		// Also include high-scoring articles regardless of topic
		if article.Score > 200 {
			relevant = true
		}

		if relevant {
			interesting = append(interesting, article)
		}
	}

	// Limit to top 5 per fetch
	if len(interesting) > 5 {
		interesting = interesting[:5]
	}

	return interesting
}

// Save stores an article in Tony's reading list.
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
