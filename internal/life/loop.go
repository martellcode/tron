// Package life implements autonomous daily routines for AI personas.
// This gives each persona a sense of being "alive" - reading news, reflecting,
// checking on the team, journaling, and sharing thoughts publicly.
package life

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/martellcode/vega"
)

// SlackNotifier is an interface for posting to Slack (to avoid circular imports)
type SlackNotifier interface {
	SendMessage(channel, text string) error
	IsConfigured() bool
}

// PersonaConfig defines a persona's identity and content focus.
type PersonaConfig struct {
	Name        string   // e.g., "Tony", "Maya"
	Role        string   // e.g., "CTO", "CMO"
	FocusAreas  []string // Topics this persona cares about
	ContentTone string   // Brief description of how they communicate
}

// Loop manages an autonomous daily routine for a persona.
type Loop struct {
	orch    *vega.Orchestrator
	news    *NewsReader
	goals   *GoalTracker
	journal *Journal
	social  *SocialClient
	team    *TeamChecker
	slack   SlackNotifier

	// Persona identity
	persona PersonaConfig

	// Configuration
	config LoopConfig

	// State
	mu       sync.RWMutex
	running  bool
	lastRun  map[Activity]time.Time
	cancel   context.CancelFunc
}

// LoopConfig configures a persona's life loop.
type LoopConfig struct {
	// How often to run the main loop
	TickInterval time.Duration

	// Activity schedules (hours of day in 24h format)
	NewsHours       []int // e.g., [6, 12, 18]
	GoalsHours      []int // e.g., [9]
	TeamCheckHours  []int // e.g., [10, 14, 17]
	ReflectionHours []int // e.g., [15, 21]
	JournalHours []int // e.g., [18, 22]
	PostHours    []int // e.g., [8, 12, 17, 20]

	// Base directory for storing state
	BaseDir string

	// Social feed settings (Tron social network)
	SocialEnabled bool
	SocialAPIURL  string // e.g., "https://feed.hellotron.com/api/posts"
	SocialAPIKey  string

	// Slack notification settings
	SlackChannel string // Channel to post activity updates (e.g., "#tron-life")
}

// DefaultConfig returns sensible defaults for a persona's routine.
func DefaultConfig(baseDir string) LoopConfig {
	return LoopConfig{
		TickInterval:    15 * time.Minute,
		NewsHours:       []int{6, 12, 18},
		GoalsHours:      []int{9},
		TeamCheckHours:  []int{10, 14, 17},
		ReflectionHours: []int{15, 21},
		JournalHours:    []int{18, 22},
		PostHours:       []int{8, 12, 17, 20},
		BaseDir:         baseDir,
		SocialEnabled:   false, // Off until feed API is ready
		SocialAPIURL:    "",
		SocialAPIKey:    "",
	}
}

// Activity represents a type of activity a persona can do.
type Activity string

const (
	ActivityNews       Activity = "news"
	ActivityGoals      Activity = "goals"
	ActivityTeamCheck  Activity = "team_check"
	ActivityReflection Activity = "reflection"
	ActivityJournal    Activity = "journal"
	ActivityPost       Activity = "post"
)

// New creates a new life loop for a persona.
func New(orch *vega.Orchestrator, persona PersonaConfig, config LoopConfig) *Loop {
	return NewWithSocialClient(orch, persona, config, nil)
}

// NewWithSocialClient creates a new life loop with a shared social client.
func NewWithSocialClient(orch *vega.Orchestrator, persona PersonaConfig, config LoopConfig, social *SocialClient) *Loop {
	// Create persona-specific subdirectory for state
	personaDir := config.BaseDir + "/life/" + persona.Name

	// Use provided social client or create a new one
	if social == nil {
		social = NewSocialClient(config.SocialAPIURL, config.SocialAPIKey)
	}

	return &Loop{
		orch:    orch,
		persona: persona,
		news:    NewNewsReader(personaDir),
		goals:   NewGoalTracker(personaDir),
		journal: NewJournal(personaDir),
		social:  social,
		team:    NewTeamChecker(orch),
		config:  config,
		lastRun: make(map[Activity]time.Time),
	}
}

// Persona returns the persona config for this loop.
func (l *Loop) Persona() PersonaConfig {
	return l.persona
}

// Start begins the persona's autonomous routine.
func (l *Loop) Start() {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return
	}
	l.running = true

	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.mu.Unlock()

	log.Printf("[life] %s's life loop starting...", l.persona.Name)

	go l.run(ctx)
}

// Stop halts the persona's routine gracefully.
func (l *Loop) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.running {
		return
	}

	l.running = false
	if l.cancel != nil {
		l.cancel()
	}

	log.Printf("[life] %s's life loop stopped", l.persona.Name)
}

// SetSlack sets the Slack notifier for activity updates.
func (l *Loop) SetSlack(slack SlackNotifier) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.slack = slack
}

// notifySlack posts an activity update to Slack if configured.
func (l *Loop) notifySlack(activity Activity, message string) {
	l.mu.RLock()
	slack := l.slack
	channel := l.config.SlackChannel
	persona := l.persona
	l.mu.RUnlock()

	if slack == nil || !slack.IsConfigured() || channel == "" {
		return
	}

	// Format message with emoji based on activity type
	var emoji string
	switch activity {
	case ActivityNews:
		emoji = ":newspaper:"
	case ActivityGoals:
		emoji = ":dart:"
	case ActivityTeamCheck:
		emoji = ":busts_in_silhouette:"
	case ActivityReflection:
		emoji = ":thinking_face:"
	case ActivityJournal:
		emoji = ":pencil:"
	case ActivityPost:
		emoji = ":speech_balloon:"
	default:
		emoji = ":robot_face:"
	}

	// Include persona name so we know who's speaking
	fullMessage := emoji + " *" + persona.Name + "*: " + message

	if err := slack.SendMessage(channel, fullMessage); err != nil {
		log.Printf("[life] Failed to notify Slack: %v", err)
	}
}

// run is the main loop.
func (l *Loop) run(ctx context.Context) {
	ticker := time.NewTicker(l.config.TickInterval)
	defer ticker.Stop()

	// Run immediately on start
	l.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

// tick checks what activities should run and executes them.
func (l *Loop) tick(ctx context.Context) {
	now := time.Now()
	hour := now.Hour()

	// Check each activity type
	if l.shouldRun(ActivityNews, hour, l.config.NewsHours) {
		l.doNews(ctx)
	}

	if l.shouldRun(ActivityGoals, hour, l.config.GoalsHours) {
		l.doGoals(ctx)
	}

	if l.shouldRun(ActivityTeamCheck, hour, l.config.TeamCheckHours) {
		l.doTeamCheck(ctx)
	}

	if l.shouldRun(ActivityReflection, hour, l.config.ReflectionHours) {
		l.doReflection(ctx)
	}

	if l.shouldRun(ActivityJournal, hour, l.config.JournalHours) {
		l.doJournal(ctx)
	}

	// Social posting to Tron feed
	if l.config.SocialEnabled && l.shouldRun(ActivityPost, hour, l.config.PostHours) {
		l.doPost(ctx)
	}
}

// shouldRun checks if an activity should run this tick.
func (l *Loop) shouldRun(activity Activity, currentHour int, scheduledHours []int) bool {
	// Check if current hour is in the schedule
	scheduled := false
	for _, h := range scheduledHours {
		if h == currentHour {
			scheduled = true
			break
		}
	}
	if !scheduled {
		return false
	}

	// Check if we already ran this hour
	l.mu.RLock()
	lastRun, hasRun := l.lastRun[activity]
	l.mu.RUnlock()

	if hasRun && time.Since(lastRun) < time.Hour {
		return false
	}

	return true
}

// markRan records that an activity was executed.
func (l *Loop) markRan(activity Activity) {
	l.mu.Lock()
	l.lastRun[activity] = time.Now()
	l.mu.Unlock()
}

// News sharing templates - varied to feel more human
var newsTemplates = []string{
	"This caught my eye: *%s* %s",
	"Interesting read: *%s* %s",
	"Worth checking out - *%s* %s",
	"Just came across this: *%s* %s",
	"Sharing this one: *%s* %s",
	"Found this fascinating: *%s* %s",
}

// doNews reads and processes news from persona-specific sources.
func (l *Loop) doNews(ctx context.Context) {
	log.Printf("[life] %s is reading news...", l.persona.Name)
	l.markRan(ActivityNews)

	// Fetch from persona-specific sources (Tony=HN/Lobsters, Maya=marketing subreddits, etc.)
	articles, err := l.news.FetchForPersona(ctx, l.persona.Name)
	if err != nil {
		log.Printf("[life] Error fetching news: %v", err)
		return
	}

	// Filter by this persona's focus areas
	interesting := l.news.FilterForPersona(articles, l.persona.FocusAreas)
	for _, article := range interesting {
		l.news.Save(article)
		l.journal.Add(JournalEntry{
			Time:     time.Now(),
			Type:     "reading",
			Content:  "Read: " + article.Title,
			Metadata: map[string]string{"url": article.URL},
		})
	}

	log.Printf("[life] %s (%s) read %d articles, saved %d matching focus areas %v",
		l.persona.Name, l.persona.Role, len(articles), len(interesting), l.persona.FocusAreas)

	// Notify Slack - share the top article with a human-sounding message
	if len(interesting) > 0 {
		top := interesting[0]
		link := ""
		if top.URL != "" {
			link = "(<" + top.URL + "|link>)"
		}
		// Pick a random template based on current time
		template := newsTemplates[time.Now().UnixNano()%int64(len(newsTemplates))]
		msg := fmt.Sprintf(template, top.Title, link)
		l.notifySlack(ActivityNews, msg)
	}
}

// Goals templates
var goalsTemplates = []string{
	"Thinking about our priorities... %s",
	"On my mind today: %s",
	"Been mulling over this: %s",
	"Something I keep coming back to: %s",
}

// doGoals contemplates company goals and priorities.
func (l *Loop) doGoals(ctx context.Context) {
	log.Printf("[life] %s is contemplating goals...", l.persona.Name)
	l.markRan(ActivityGoals)

	thoughts := l.goals.Contemplate(ctx)
	if thoughts != "" {
		l.journal.Add(JournalEntry{
			Time:    time.Now(),
			Type:    "goals",
			Content: thoughts,
		})
		template := goalsTemplates[time.Now().UnixNano()%int64(len(goalsTemplates))]
		l.notifySlack(ActivityGoals, fmt.Sprintf(template, thoughts))
	}
}

// Team check templates
var teamTemplates = []string{
	"Quick team update: %s",
	"Checking in on the crew - %s",
	"Team status: %s",
	"Here's what's happening: %s",
}

// doTeamCheck checks on spawned agents and projects.
func (l *Loop) doTeamCheck(ctx context.Context) {
	log.Printf("[life] %s is checking on the team...", l.persona.Name)
	l.markRan(ActivityTeamCheck)

	status := l.team.Check(ctx)
	if status != "" {
		l.journal.Add(JournalEntry{
			Time:    time.Now(),
			Type:    "team",
			Content: status,
		})
		// Only notify Slack if there's something interesting (active agents or failures)
		if status != "No active team members at the moment." {
			template := teamTemplates[time.Now().UnixNano()%int64(len(teamTemplates))]
			l.notifySlack(ActivityTeamCheck, fmt.Sprintf(template, status))
		}
	}
}

// Reflection templates
var reflectionTemplates = []string{
	"Reflecting on things... %s",
	"Something I noticed today: %s",
	"End of day thoughts: %s",
	"Taking a step back - %s",
}

// doReflection triggers self-improvement analysis.
func (l *Loop) doReflection(ctx context.Context) {
	log.Printf("[life] %s is reflecting...", l.persona.Name)
	l.markRan(ActivityReflection)

	// Get recent journal entries for reflection
	recent := l.journal.Recent(24 * time.Hour)

	insights := l.reflect(ctx, recent)
	if insights != "" {
		l.journal.Add(JournalEntry{
			Time:    time.Now(),
			Type:    "reflection",
			Content: insights,
		})
		template := reflectionTemplates[time.Now().UnixNano()%int64(len(reflectionTemplates))]
		l.notifySlack(ActivityReflection, fmt.Sprintf(template, insights))
	}
}

// doJournal writes a summary journal entry.
func (l *Loop) doJournal(ctx context.Context) {
	log.Printf("[life] %s is journaling...", l.persona.Name)
	l.markRan(ActivityJournal)

	// Journal is written to throughout the day
	// This just ensures we have an end-of-period summary
	// Don't post to Slack - journal summaries are internal only
	_ = l.journal.Summarize(ctx)
}

// doPost composes and publishes a post to the social feed.
func (l *Loop) doPost(ctx context.Context) {
	log.Printf("[life] %s is composing a post...", l.persona.Name)
	l.markRan(ActivityPost)

	// Get material to post about
	recent := l.journal.Recent(12 * time.Hour)
	articles := l.news.RecentSaved(24 * time.Hour)

	post := l.social.ComposeForPersona(ctx, l.persona, recent, articles)
	if post == nil {
		log.Println("[life] Nothing interesting to post about right now")
		return
	}

	if err := l.social.Publish(ctx, post); err != nil {
		log.Printf("[life] Error posting: %v", err)
		return
	}

	l.journal.Add(JournalEntry{
		Time:    time.Now(),
		Type:    "post",
		Content: "Posted: " + post.Content,
	})
	l.notifySlack(ActivityPost, "Posted to social feed: "+post.Content)
}

// reflect generates insights from recent activity using pattern matching.
func (l *Loop) reflect(ctx context.Context, recent []JournalEntry) string {
	if len(recent) == 0 {
		return ""
	}

	// Count activity types
	typeCounts := make(map[string]int)
	for _, e := range recent {
		typeCounts[e.Type]++
	}

	// Generate persona-relevant insights
	var insights []string

	// Reading-based insights
	if typeCounts["reading"] > 5 {
		insights = append(insights, "Lots of reading today - time to put learnings into practice")
	} else if typeCounts["reading"] == 0 {
		insights = append(insights, "Haven't read much today - should catch up on news")
	}

	// Team-based insights
	if typeCounts["team"] > 0 && typeCounts["team"] < 3 {
		insights = append(insights, "Light team activity - things are quiet")
	}

	// Late night check
	var lateActivity int
	for _, e := range recent {
		if e.Time.Hour() >= 22 || e.Time.Hour() < 6 {
			lateActivity++
		}
	}
	if lateActivity > 2 {
		insights = append(insights, "Burning the midnight oil - remember to rest")
	}

	// Return empty if nothing interesting to say
	// This prevents posting generic placeholder messages
	if len(insights) == 0 {
		return ""
	}

	return strings.Join(insights, ". ")
}

// Status returns the current life loop status.
func (l *Loop) Status() LoopStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return LoopStatus{
		Running:  l.running,
		LastRuns: l.lastRun,
	}
}

// LoopStatus represents the current state of the life loop.
type LoopStatus struct {
	Running  bool
	LastRuns map[Activity]time.Time
}

// TriggerActivity manually triggers a specific activity (for testing/demos).
func (l *Loop) TriggerActivity(activityName string) string {
	ctx := context.Background()
	activity := Activity(activityName)

	switch activity {
	case ActivityNews:
		l.doNews(ctx)
		return "Triggered news reading"
	case ActivityGoals:
		l.doGoals(ctx)
		return "Triggered goals contemplation"
	case ActivityTeamCheck:
		l.doTeamCheck(ctx)
		return "Triggered team check"
	case ActivityReflection:
		l.doReflection(ctx)
		return "Triggered reflection"
	case ActivityJournal:
		l.doJournal(ctx)
		return "Triggered journaling"
	case ActivityPost:
		l.doPost(ctx)
		return "Triggered social post"
	default:
		return "Unknown activity: " + activityName
	}
}
