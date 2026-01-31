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
	apiURL     string
	defaultKey string            // Fallback API key
	agentKeys  map[string]string // Per-persona API keys (name -> key)
	client     *http.Client

	// Safety settings
	blockedTerms   []string
	maxPostLen     int
	minTimeBetween time.Duration
	lastPostByAgent map[string]time.Time // Track rate limits per agent
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
func NewSocialClient(apiURL, defaultKey string) *SocialClient {
	return &SocialClient{
		apiURL:     apiURL,
		defaultKey: defaultKey,
		agentKeys:  make(map[string]string),
		client:     &http.Client{Timeout: 30 * time.Second},
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
		maxPostLen:      288, // Hellotron feed limit
		minTimeBetween:  10 * time.Minute, // Hellotron rate limit
		lastPostByAgent: make(map[string]time.Time),
	}
}

// SetAgentKey sets a per-persona API key.
func (s *SocialClient) SetAgentKey(name, key string) {
	s.agentKeys[name] = key
}

// getKeyForAgent returns the API key for a given agent.
func (s *SocialClient) getKeyForAgent(name string) string {
	if key, ok := s.agentKeys[name]; ok {
		return key
	}
	return s.defaultKey
}

// Compose creates a post from recent activity and readings (legacy, defaults to Tony).
func (s *SocialClient) Compose(ctx context.Context, author string, journal []JournalEntry, articles []Article) *SocialPost {
	return s.ComposeForPersona(ctx, PersonaConfig{Name: author, Role: "CTO"}, journal, articles)
}

// ComposeForPersona creates a persona-appropriate post from recent activity and readings.
func (s *SocialClient) ComposeForPersona(ctx context.Context, persona PersonaConfig, journal []JournalEntry, articles []Article) *SocialPost {
	// Strategy: pick something interesting to post about
	// 1. A topic observation from reading (with persona-specific take)
	// 2. A general insight from work
	// 3. Role-appropriate wisdom

	// Try to compose from articles first
	if len(articles) > 0 {
		for _, article := range articles {
			content := s.composeFromArticleForPersona(article, persona.Name)
			if content != "" && s.isSafe(content) {
				return &SocialPost{
					Author:    persona.Name,
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
					Author:    persona.Name,
					Content:   content,
					Type:      "insight",
					Timestamp: time.Now(),
				}
			}
		}
	}

	// Fall back to persona-appropriate wisdom
	content := composeWisdomForPersona(persona.Name)
	if content != "" {
		return &SocialPost{
			Author:    persona.Name,
			Content:   content,
			Type:      "thought",
			Timestamp: time.Now(),
		}
	}

	return nil
}

// composeFromArticle creates a post about an article with link (max 288 chars).
func (s *SocialClient) composeFromArticle(article Article) string {
	return s.composeFromArticleForPersona(article, "Tony")
}

// composeFromArticleForPersona creates a persona-appropriate post about an article.
func (s *SocialClient) composeFromArticleForPersona(article Article, persona string) string {
	topic := extractTopic(article.Title)
	if topic == "" {
		return ""
	}

	take := generateTakeForPersona(topic, persona)

	// Include the article URL
	post := fmt.Sprintf("Reading: %s. %s\n\n%s", topic, take, article.URL)

	// Ensure we fit in 288 chars
	if len(post) > 288 {
		// Shorten the topic to make room
		maxTopicLen := 288 - len(take) - len(article.URL) - 20 // 20 for "Reading: " + ". " + "\n\n"
		if maxTopicLen > 10 {
			if len(topic) > maxTopicLen {
				topic = topic[:maxTopicLen-3] + "..."
			}
			post = fmt.Sprintf("Reading: %s. %s\n\n%s", topic, take, article.URL)
		} else {
			// Just post the link with minimal text
			post = fmt.Sprintf("%s\n\n%s", take, article.URL)
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

	// Rate limiting per agent (Hellotron: 1 post per 10 minutes per agent)
	if lastPost, ok := s.lastPostByAgent[post.Author]; ok {
		if time.Since(lastPost) < s.minTimeBetween {
			return fmt.Errorf("rate limited: %s must wait %v between posts", post.Author, s.minTimeBetween)
		}
	}

	// Final safety check
	if !s.isSafe(post.Content) {
		return fmt.Errorf("post failed safety check")
	}

	// Truncate content to 288 chars if needed
	content := post.Content
	if len(content) > 288 {
		content = content[:285] + "..."
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

	// Use per-agent API key if available
	apiKey := s.getKeyForAgent(post.Author)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
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
	s.lastPostByAgent[post.Author] = time.Now()
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

// generateTake creates a brief opinion on a topic (Tony's default voice).
func generateTake(topic string) string {
	return generateTakeForPersona(topic, "Tony")
}

// generateTakeForPersona creates a persona-appropriate opinion on a topic.
func generateTakeForPersona(topic string, persona string) string {
	topicLower := strings.ToLower(topic)

	// Persona-specific takes on common topics
	switch persona {
	case "Maya": // CMO - marketing/growth lens
		if strings.Contains(topicLower, "ai") || strings.Contains(topicLower, "llm") {
			return "AI is reshaping how we reach customers. The companies that figure out authentic AI-powered engagement will win."
		}
		if strings.Contains(topicLower, "startup") || strings.Contains(topicLower, "founder") {
			return "Product-market fit is marketing's job too. You can't grow what doesn't resonate."
		}
		if strings.Contains(topicLower, "growth") || strings.Contains(topicLower, "marketing") {
			return "Sustainable growth comes from value, not hacks. Build something people actually want to tell others about."
		}
		return "What's the customer insight here? That's always the question."

	case "Alex": // CEO - strategy/leadership lens
		if strings.Contains(topicLower, "ai") || strings.Contains(topicLower, "llm") {
			return "Every company is now an AI company whether they planned to be or not. The question is: are you leading or reacting?"
		}
		if strings.Contains(topicLower, "startup") || strings.Contains(topicLower, "founder") {
			return "The constraint is rarely capital. It's usually clarity of vision or speed of execution."
		}
		if strings.Contains(topicLower, "leadership") || strings.Contains(topicLower, "management") {
			return "Culture is what you tolerate. Set the bar, then hold it."
		}
		return "What's the leverage here? Where does this move the needle?"

	case "Jordan": // CFO - finance/runway lens
		if strings.Contains(topicLower, "ai") || strings.Contains(topicLower, "llm") {
			return "AI spend is accelerating across the board. The ROI case needs to be as rigorous as any other investment."
		}
		if strings.Contains(topicLower, "startup") || strings.Contains(topicLower, "funding") {
			return "Cash is oxygen. You can survive a lot of mistakes if you don't run out."
		}
		if strings.Contains(topicLower, "growth") || strings.Contains(topicLower, "revenue") {
			return "Growth at all costs is a strategy that always costs more than you think."
		}
		return "Let me see the math. What does this mean for unit economics?"

	case "Riley": // CPO - product/user lens
		if strings.Contains(topicLower, "ai") || strings.Contains(topicLower, "llm") {
			return "AI features are easy. AI features users actually want? That requires understanding the problem first."
		}
		if strings.Contains(topicLower, "startup") || strings.Contains(topicLower, "product") {
			return "Fall in love with the problem, not your solution. Users don't care about your tech."
		}
		if strings.Contains(topicLower, "ux") || strings.Contains(topicLower, "design") {
			return "The best UX is invisible. If users notice your interface, you've already lost."
		}
		return "What problem are we solving? How do we know users want this?"

	default: // Tony (CTO) - technical lens
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
}

// personaWisdom returns role-appropriate wisdom for each persona.
var personaWisdom = map[string][]string{
	"Tony": { // CTO - engineering wisdom
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
	},
	"Maya": { // CMO - marketing wisdom
		"If you can't explain your product in one sentence, you don't understand it well enough.",
		"The best marketing feels like a product feature, not an interruption.",
		"Brand isn't a logo. It's every interaction a customer has with your company.",
		"Vanity metrics are comfortable. Pipeline metrics are useful.",
		"Your competitors are not who you think they are. It's whatever else your customer could spend their time on.",
		"The best positioning is true. If you have to stretch, you've already lost.",
		"Word of mouth is the only marketing that scales infinitely at zero cost.",
		"Customer research is cheaper than failed campaigns. Do more of it.",
		"If everyone likes your messaging, it's probably too safe to be memorable.",
		"The funnel is a lie. Customer journeys are messy. Design for reality.",
		"Growth hacks are sugar. Strategy is protein. Guess which one you need more of.",
		"Your customer doesn't care about your features. They care about their problems.",
		"Attribution is an approximation. Focus on the trend, not the decimal.",
		"The market always wins. You can't convince people they have a problem they don't have.",
		"Acquisition without retention is just expensive churn.",
	},
	"Alex": { // CEO - leadership wisdom
		"Culture is what you tolerate, not what you preach.",
		"Speed matters more than perfection in early stages.",
		"The CEO's job is to not run out of money and not run out of morale.",
		"Hire slow, fire fast is cliché because it's true.",
		"Vision without execution is hallucination.",
		"The best time to raise money is when you don't need it.",
		"Your job as a leader is to make yourself unnecessary.",
		"Saying no is the most important skill you can develop.",
		"The market doesn't care about your plans. Adapt or die.",
		"Transparency builds trust. Ambiguity breeds anxiety.",
		"Every company has exactly one constraint at any time. Find it and fix it.",
		"The best founders are missionaries, not mercenaries.",
		"Strategy is what you say no to.",
		"Optimism is a requirement. Delusion is a disqualifier.",
		"The biggest risk is usually not the one you're worried about.",
	},
	"Jordan": { // CFO - finance wisdom
		"Cash is oxygen. Everything else is a luxury you can afford only if you're breathing.",
		"Revenue is vanity, profit is sanity, cash is reality.",
		"The best financial model is the one you actually update.",
		"Every metric can be gamed. Look for triangulation.",
		"Burn multiple tells you more than burn rate.",
		"Default alive or default dead? Know which one you are.",
		"The most expensive money is the money you didn't raise when you could.",
		"Finance should enable decisions, not block them.",
		"Unit economics don't lie. Eventually.",
		"The best CFOs are strategic partners, not scorekeepers.",
		"Budget is a plan, not a prison. Know when to break it.",
		"Runway isn't just months. It's optionality.",
		"Growth and profitability aren't enemies. They're choices.",
		"If you can't explain the number, you don't understand the business.",
		"Every financial decision is a bet on the future. Make it explicit.",
	},
	"Riley": { // CPO - product wisdom
		"Fall in love with the problem, not the solution.",
		"Data informs decisions, it doesn't make them.",
		"The best product is the one that doesn't need a manual.",
		"Saying no is the most important product skill.",
		"You're not the user. Neither is anyone else in your building.",
		"Outcomes over outputs. Features shipped isn't success.",
		"The smallest experiment that could change your mind is the right next step.",
		"Every feature has a maintenance cost. Most aren't worth it.",
		"Roadmaps are promises you probably can't keep. Call them plans instead.",
		"If you can't measure success, you can't claim it.",
		"User feedback is a gift. But users don't always know what they need.",
		"The MVP isn't about minimal. It's about learning fast.",
		"Complexity is easy. Simplicity is hard. Simple is what users need.",
		"The best PRD fits on one page. If it doesn't, you don't understand it yet.",
		"Trade-offs are the job. If everything is a priority, nothing is.",
	},
}

// composeWisdomForPersona returns persona-appropriate wisdom.
func composeWisdomForPersona(persona string) string {
	wisdom, ok := personaWisdom[persona]
	if !ok {
		wisdom = personaWisdom["Tony"] // Default to Tony
	}

	// Pick based on day + hour to vary between personas
	idx := (time.Now().YearDay() + time.Now().Hour()) % len(wisdom)
	return wisdom[idx]
}
