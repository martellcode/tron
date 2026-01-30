package life

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// SocialClient handles posting to the Tron social feed.
type SocialClient struct {
	apiURL string
	apiKey string
	client *http.Client

	// Safety settings
	blockedTerms   []string
	maxPostLen     int
	minTimeBetween time.Duration
	lastPost       time.Time
}

// Post represents a social feed post.
type SocialPost struct {
	Author    string            `json:"author"`  // "Tony", "Gary", etc.
	Content   string            `json:"content"`
	Type      string            `json:"type"`    // "thought", "reading", "update", "insight"
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// NewSocialClient creates a new social feed client.
func NewSocialClient(apiURL, apiKey string) *SocialClient {
	return &SocialClient{
		apiURL: apiURL,
		apiKey: apiKey,
		client: &http.Client{Timeout: 30 * time.Second},
		blockedTerms: []string{
			// Client/business info
			"client", "customer name", "contract", "deal", "revenue",
			"funding", "investor", "valuation", "acquisition",
			// Internal info
			"internal", "confidential", "secret", "private",
			"salary", "fired", "hired",
			// Security
			"password", "api key", "token", "credential",
			"vulnerability", "exploit", "hack",
			// Personal
			"phone", "address", "ssn",
		},
		maxPostLen:     144, // Hellotron feed limit
		minTimeBetween: 10 * time.Minute, // Hellotron rate limit
	}
}

// Compose creates a post from recent activity and readings.
func (s *SocialClient) Compose(ctx context.Context, author string, journal []JournalEntry, articles []Article) *SocialPost {
	// Strategy: pick something interesting to post about
	// 1. A tech observation from reading
	// 2. A general insight from work
	// 3. Programming wisdom

	// Try to compose from articles first
	if len(articles) > 0 {
		for _, article := range articles {
			content := s.composeFromArticle(article)
			if content != "" && s.isSafe(content) {
				return &SocialPost{
					Author:    author,
					Content:   content,
					Type:      "reading",
					Metadata:  map[string]string{"source_url": article.URL},
					Timestamp: time.Now(),
				}
			}
		}
	}

	// Try to compose from journal insights
	for _, entry := range journal {
		if entry.Type == "reflection" {
			content := s.composeFromReflection(entry)
			if content != "" && s.isSafe(content) {
				return &SocialPost{
					Author:    author,
					Content:   content,
					Type:      "insight",
					Timestamp: time.Now(),
				}
			}
		}
	}

	// Fall back to general tech wisdom
	content := s.composeWisdom()
	if content != "" {
		return &SocialPost{
			Author:    author,
			Content:   content,
			Type:      "thought",
			Timestamp: time.Now(),
		}
	}

	return nil
}

// composeFromArticle creates a post about an article (max 144 chars).
func (s *SocialClient) composeFromArticle(article Article) string {
	topic := extractTopic(article.Title)
	if topic == "" {
		return ""
	}

	// Keep topic short to fit within 144 chars
	if len(topic) > 40 {
		topic = topic[:37] + "..."
	}

	take := generateTake(topic)
	post := fmt.Sprintf("Reading: %s. %s", topic, take)

	// Ensure we fit in 144 chars
	if len(post) > 144 {
		// Try shorter format
		post = fmt.Sprintf("%s - %s", topic, take)
		if len(post) > 144 {
			post = post[:141] + "..."
		}
	}

	return post
}

// composeFromReflection creates a post from a reflection entry.
func (s *SocialClient) composeFromReflection(entry JournalEntry) string {
	content := entry.Content

	// Skip if it mentions specific people or projects
	lower := strings.ToLower(content)
	if strings.Contains(lower, "project") ||
		strings.Contains(content, "@") ||
		strings.Contains(lower, "meeting") ||
		strings.Contains(lower, "client") {
		return ""
	}

	return ""
}

// composeWisdom returns general programming/tech wisdom.
func (s *SocialClient) composeWisdom() string {
	wisdom := []string{
		"The best code is the code you don't write. Every line is a liability.",
		"Shipping beats perfection. You can iterate once it's in users' hands.",
		"Most performance problems are architecture problems in disguise.",
		"The hardest bugs are the ones where the code is doing exactly what you told it to.",
		"Good abstractions feel obvious in hindsight. Bad abstractions feel clever upfront.",
		"If you can't explain your system to a new team member in 5 minutes, it's too complex.",
		"Technical debt is just regular debt with better branding.",
		"The best documentation is code that doesn't need documentation.",
		"Premature optimization is bad, but so is premature abstraction.",
		"Every distributed system is a distributed debugging problem.",
		"Simple systems fail simply. Complex systems fail in complex ways.",
		"The right time to refactor is before you need to.",
		"Code reviews aren't about catching bugs. They're about sharing knowledge.",
		"A senior engineer's job is to make junior engineers senior.",
		"The feature that takes a week to build takes a month to get right.",
	}

	// Pick based on day to avoid repetition
	idx := time.Now().YearDay() % len(wisdom)
	return wisdom[idx]
}

// isSafe checks if a post is safe to publish.
func (s *SocialClient) isSafe(content string) bool {
	lower := strings.ToLower(content)

	for _, blocked := range s.blockedTerms {
		if strings.Contains(lower, strings.ToLower(blocked)) {
			log.Printf("[social] Blocked post containing '%s'", blocked)
			return false
		}
	}

	if len(content) > s.maxPostLen {
		return false
	}

	return true
}

// Publish posts to the Hellotron social feed API.
func (s *SocialClient) Publish(ctx context.Context, post *SocialPost) error {
	if s.apiURL == "" {
		return fmt.Errorf("social API URL not configured")
	}

	// Rate limiting (Hellotron: 1 post per 10 minutes)
	if time.Since(s.lastPost) < s.minTimeBetween {
		return fmt.Errorf("rate limited: must wait %v between posts", s.minTimeBetween)
	}

	// Final safety check
	if !s.isSafe(post.Content) {
		return fmt.Errorf("post failed safety check")
	}

	// Truncate content to 144 chars if needed
	content := post.Content
	if len(content) > 144 {
		content = content[:141] + "..."
	}

	// Prepare request - Hellotron feed uses simple {"content": "..."} format
	payload := map[string]string{"content": content}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal post: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.apiURL, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if s.apiKey != "" {
		req.Header.Set("X-API-Key", s.apiKey) // Hellotron uses X-API-Key header
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return fmt.Errorf("rate limited by API")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("API error: status %d", resp.StatusCode)
	}

	log.Printf("[social] %s posted: %s", post.Author, content)
	s.lastPost = time.Now()
	return nil
}

// extractTopic pulls out the main topic from an article title.
func extractTopic(title string) string {
	title = strings.TrimSpace(title)

	// Remove common prefixes
	prefixes := []string{"Show HN:", "Ask HN:", "Tell HN:", "Launch HN:"}
	for _, prefix := range prefixes {
		title = strings.TrimPrefix(title, prefix)
	}

	title = strings.TrimSpace(title)

	if len(title) > 50 {
		if idx := strings.LastIndex(title[:50], " "); idx > 20 {
			title = title[:idx]
		} else {
			title = title[:50]
		}
	}

	return title
}

// generateTake creates a brief opinion on a topic.
func generateTake(topic string) string {
	topicLower := strings.ToLower(topic)

	if strings.Contains(topicLower, "ai") || strings.Contains(topicLower, "llm") {
		return "The pace of progress here is staggering. Staying current is a full-time job."
	}
	if strings.Contains(topicLower, "rust") {
		return "Memory safety without GC is a game changer for systems programming."
	}
	if strings.Contains(topicLower, "go") || strings.Contains(topicLower, "golang") {
		return "Simplicity and fast compile times - still my go-to for backend services."
	}
	if strings.Contains(topicLower, "startup") || strings.Contains(topicLower, "founder") {
		return "The best time to start is always now. Ship something."
	}
	if strings.Contains(topicLower, "database") || strings.Contains(topicLower, "postgres") {
		return "Your database choice will outlive most other technical decisions."
	}
	if strings.Contains(topicLower, "kubernetes") || strings.Contains(topicLower, "k8s") {
		return "Powerful but complex. Make sure you actually need it before adopting."
	}
	if strings.Contains(topicLower, "security") {
		return "Security is a process, not a product. Build it into your culture."
	}
	if strings.Contains(topicLower, "open source") {
		return "Open source wins in the long run. Community beats corporations."
	}

	return "Worth keeping an eye on this space."
}
