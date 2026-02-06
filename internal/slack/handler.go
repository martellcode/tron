package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/everydev1618/tron/internal/knowledge"
	"github.com/everydev1618/tron/internal/memory"
	"github.com/everydev1618/tron/internal/notification"
	"github.com/everydev1618/govega"
	"github.com/everydev1618/govega/dsl"
)

// xmlTagPattern matches XML-style tags used for tool calls and results
var xmlTagPattern = regexp.MustCompile(`(?s)<[a-z_:]+[^>]*>.*?</[a-z_:]+>`)

// personaPrefixPattern matches "persona:" or "persona," at the start of a message (case-insensitive)
var personaPrefixPattern = regexp.MustCompile(`(?i)^(tony|maya|alex|jordan|riley)[,:]\s*`)

// routerResponsePattern extracts the persona name from Tron's routing decision
var routerResponsePattern = regexp.MustCompile(`(?i)\b(tony|maya|alex|jordan|riley)\b`)

const (
	maxToolLoops            = 10
	slackSynthesisDelay     = 30 * time.Minute
	synthesisCheckPeriod    = 5 * time.Minute
	eventCacheTTL           = 1 * time.Hour
	timestampValidityWindow = 5 * time.Minute
)

// tronRouterPrompt is the system prompt for Tron when acting as a router
const tronRouterPrompt = `You are Tron, the team lead for a C-suite AI team at Hellotron. Your job is to route incoming questions to the right team member.

Your team:
- Tony (CTO): Technology, architecture, engineering, code, technical implementation
- Maya (CMO): Marketing, brand, messaging, customer insights, content strategy
- Alex (CFO): Finance, metrics, ROI, budgets, financial analysis
- Jordan (COO): Operations, processes, scaling, logistics, efficiency
- Riley (CPO): Product, UX, features, roadmap, user experience

Analyze the user's message and respond with ONLY the name of the team member who should handle it.
If the question spans multiple domains, pick the primary one.
If it's a general greeting or unclear, respond with "Tony" as the default.

Respond with just the name, nothing else. Example: "Maya"`

// conversationMessage represents a message in conversation history
type conversationMessage struct {
	Role    string
	Content string
}

// Handler handles Slack events
type Handler struct {
	client        *Client
	signingSecret string
	orch          *vega.Orchestrator
	config        *dsl.Document
	baseDir       string

	// Persona this handler is for (empty means use routing)
	persona string

	// Conversation tracking
	conversations   map[string][]conversationMessage // channel -> messages
	conversationsMu sync.RWMutex

	// Activity tracking for synthesis
	lastActivity  map[string]time.Time
	lastSynthesis map[string]time.Time
	activityMu    sync.RWMutex

	// User cache
	userCache   map[string]*User
	userCacheMu sync.RWMutex

	// Channel cache (ID -> name)
	channelCache   map[string]string
	channelCacheMu sync.RWMutex

	// Event deduplication
	processedEvents   map[string]time.Time
	processedEventsMu sync.RWMutex

	// Per-channel processing lock to prevent concurrent agent spawns
	channelProcessing   map[string]bool
	channelProcessingMu sync.Mutex

	// Session management - maps channel ID to persistent process
	sessions   map[string]*vega.Process
	sessionsMu sync.RWMutex

	// Tool execution
	tools       *vega.Tools
	customTools interface{} // *tools.PersonaTools

	// Knowledge store for feed injection
	knowledgeStore *knowledge.Store

	// Lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewHandler creates a new Slack event handler (legacy - uses routing)
func NewHandler(client *Client, signingSecret string, orch *vega.Orchestrator, config *dsl.Document, baseDir string) *Handler {
	return NewPersonaHandler(client, signingSecret, orch, config, baseDir, "")
}

// NewPersonaHandler creates a Slack handler for a specific persona
// If persona is empty, falls back to routing logic
func NewPersonaHandler(client *Client, signingSecret string, orch *vega.Orchestrator, config *dsl.Document, baseDir, persona string) *Handler {
	h := &Handler{
		client:          client,
		signingSecret:   signingSecret,
		orch:            orch,
		config:          config,
		baseDir:         baseDir,
		persona:         persona,
		conversations:   make(map[string][]conversationMessage),
		lastActivity:    make(map[string]time.Time),
		lastSynthesis:   make(map[string]time.Time),
		userCache:       make(map[string]*User),
		channelCache:      make(map[string]string),
		processedEvents:   make(map[string]time.Time),
		channelProcessing: make(map[string]bool),
		sessions:          make(map[string]*vega.Process),
		stopCh:            make(chan struct{}),
	}

	// Start synthesis loop
	h.wg.Add(1)
	go h.synthesisLoop()

	// Start event cache cleanup
	h.wg.Add(1)
	go h.eventCacheCleanup()

	if persona != "" {
		log.Printf("[slack] Handler created for persona: %s", persona)
	}

	return h
}

// SetTools sets the tools for the handler
func (h *Handler) SetTools(tools *vega.Tools) {
	h.tools = tools
}

// Client returns the Slack client for sending messages
func (h *Handler) Client() *Client {
	return h.client
}

// SetCustomTools sets the custom persona tools
func (h *Handler) SetCustomTools(customTools interface{}) {
	h.customTools = customTools
}

// SetKnowledgeStore sets the knowledge store for feed injection
func (h *Handler) SetKnowledgeStore(store *knowledge.Store) {
	h.knowledgeStore = store
}

// getOrCreateSession returns an existing session for the channel or creates a new one
func (h *Handler) getOrCreateSession(ctx context.Context, channel, agentName, userName string) (*vega.Process, error) {
	// Check for existing running session
	h.sessionsMu.RLock()
	proc, exists := h.sessions[channel]
	h.sessionsMu.RUnlock()

	if exists && proc.Status() == vega.StatusRunning {
		return proc, nil
	}

	// Need to create a new session
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()

	// Double-check after acquiring write lock
	proc, exists = h.sessions[channel]
	if exists && proc.Status() == vega.StatusRunning {
		return proc, nil
	}

	// Get agent definition
	agentDef, ok := h.config.Agents[agentName]
	if !ok {
		return nil, fmt.Errorf("%s agent not found", agentName)
	}

	// Build system prompt with context
	systemPrompt := agentDef.System
	systemPrompt += fmt.Sprintf("\n\n## Current Context\nChannel: Slack\nUser: %s\n", userName)

	// Load memory
	memContent, _ := memory.Load(h.baseDir)
	if memContent != "" {
		systemPrompt += memory.GetPromptSection(memContent)
	}

	// Inject knowledge feed
	if h.knowledgeStore != nil {
		feedSection := knowledge.GetFeedPromptSection(h.knowledgeStore)
		if feedSection != "" {
			systemPrompt += feedSection
		}
	}

	// Filter tools based on agent's config
	agentTools := h.tools
	if len(agentDef.Tools) > 0 && h.tools != nil {
		agentTools = h.tools.Filter(agentDef.Tools...)
	}

	agent := vega.Agent{
		Name:   agentDef.Name,
		Model:  agentDef.Model,
		System: vega.StaticPrompt(systemPrompt),
		Tools:  agentTools,
	}

	if agentDef.Temperature != nil {
		agent.Temperature = agentDef.Temperature
	}

	// Spawn new process
	proc, err := h.orch.Spawn(agent,
		vega.WithTask("Slack conversation"),
		vega.WithSupervision(vega.Supervision{
			Strategy:    vega.Restart,
			MaxRestarts: 3,
			Window:      600_000_000_000, // 10 minutes
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to spawn process: %w", err)
	}

	h.sessions[channel] = proc
	log.Printf("[slack] Created new session for channel %s with agent %s (process %s)", channel, agentName, proc.ID)

	return proc, nil
}

// Shutdown gracefully stops the handler
func (h *Handler) Shutdown() {
	close(h.stopCh)
	h.wg.Wait()
}

// ClearSessions clears all cached sessions, forcing new prompts on next message
func (h *Handler) ClearSessions() int {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()

	count := len(h.sessions)
	h.sessions = make(map[string]*vega.Process)
	log.Printf("[slack] Cleared %d sessions for persona %s", count, h.persona)
	return count
}

// HandleEvents is the HTTP handler for Slack events
func (h *Handler) HandleEvents(w http.ResponseWriter, r *http.Request) {
	log.Printf("[slack] Received event request from %s", r.RemoteAddr)

	// Read body for signature verification
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	// Verify request signature
	if h.signingSecret != "" {
		timestamp := r.Header.Get("X-Slack-Request-Timestamp")
		signature := r.Header.Get("X-Slack-Signature")

		if !h.verifyRequest(timestamp, signature, body) {
			log.Printf("[slack] Invalid signature - timestamp: %s", timestamp)
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Parse event
	var payload EventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[slack] Failed to parse JSON: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("[slack] Event type: %s, event_id: %s", payload.Type, payload.EventID)

	// Handle URL verification challenge
	if payload.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(payload.Challenge))
		return
	}

	// Deduplicate events
	if payload.EventID != "" {
		h.processedEventsMu.RLock()
		_, processed := h.processedEvents[payload.EventID]
		h.processedEventsMu.RUnlock()

		if processed {
			w.WriteHeader(http.StatusOK)
			return
		}

		h.processedEventsMu.Lock()
		h.processedEvents[payload.EventID] = time.Now()
		h.processedEventsMu.Unlock()
	}

	// Process event asynchronously
	if payload.Event != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Panic processing Slack event: %v", r)
				}
			}()
			h.processEvent(payload.Event)
		}()
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) verifyRequest(timestamp, signature string, body []byte) bool {
	// Check timestamp is recent
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}

	eventTime := time.Unix(ts, 0)
	if time.Since(eventTime) > timestampValidityWindow {
		return false
	}

	// Compute expected signature
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(h.signingSecret))
	mac.Write([]byte(baseString))
	expectedSig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(signature), []byte(expectedSig))
}

func (h *Handler) processEvent(event *SlackEvent) {
	// Filter events
	if event.Type != "message" && event.Type != "app_mention" {
		return
	}
	if event.IsFromBot() {
		return
	}
	if event.Subtype != "" {
		return // Ignore message edits, deletes, etc.
	}

	// In channels, only respond to @mentions (app_mention events)
	// In DMs (channel IDs starting with "D"), respond to all messages
	isDM := strings.HasPrefix(event.Channel, "D")
	if !isDM && event.Type != "app_mention" {
		return // Ignore non-mention messages in channels
	}

	// Acquire per-channel processing lock to prevent concurrent message processing
	// This ensures only one message is processed at a time per channel
	h.channelProcessingMu.Lock()
	if h.channelProcessing[event.Channel] {
		h.channelProcessingMu.Unlock()
		log.Printf("[slack] Channel %s already processing, skipping event", event.Channel)
		return
	}
	h.channelProcessing[event.Channel] = true
	h.channelProcessingMu.Unlock()

	// Ensure we release the channel lock when done
	defer func() {
		h.channelProcessingMu.Lock()
		delete(h.channelProcessing, event.Channel)
		h.channelProcessingMu.Unlock()
	}()

	// Update activity
	h.activityMu.Lock()
	h.lastActivity[event.Channel] = time.Now()
	h.activityMu.Unlock()

	// Get user info
	user := h.getUser(event.User)
	userName := "Unknown"
	userEmail := ""
	if user != nil {
		userName = user.RealName
		if userName == "" {
			userName = user.Name
		}
		userEmail = user.Email
	}

	// Create context with channel info for spawn notifications
	ctx := context.Background()
	ctx = notification.WithChannel(ctx, notification.ChannelContext{
		Type:      notification.ChannelSlack,
		ChannelID: event.Channel,
		UserID:    event.User,
		UserName:  userName,
		Email:     userEmail,
	})

	// Resolve agent based on message prefix and channel name
	channelName := h.getChannelName(event.Channel)
	agentName, cleanedMessage := h.resolveAgentFromMessage(ctx, channelName, event.Text)

	// Validate message content is not empty
	cleanedMessage = strings.TrimSpace(cleanedMessage)
	if cleanedMessage == "" {
		log.Printf("[slack] Empty message content after cleaning, skipping")
		return
	}

	// Get or create persistent session for this channel
	proc, err := h.getOrCreateSession(ctx, event.Channel, agentName, userName)
	if err != nil {
		log.Printf("Error getting/creating session: %v", err)
		h.client.SendMessage(event.Channel, "Sorry, I encountered an error processing your message.")
		return
	}

	// Send message to the persistent process
	response, err := proc.Send(ctx, cleanedMessage)
	if err != nil {
		log.Printf("Error processing Slack message: %v", err)
		// Mark the session as failed so a new one is created next time
		proc.Fail(err)
		h.client.SendMessage(event.Channel, "Sorry, I encountered an error processing your message.")
		return
	}

	// Strip tool call markup before sending to Slack
	cleanResponse := stripToolCalls(response)
	if cleanResponse == "" {
		// If response was only tool calls, don't send empty message
		log.Printf("Response contained only tool calls, skipping Slack message")
		return
	}

	// Send response
	if err := h.client.SendMessage(event.Channel, cleanResponse); err != nil {
		log.Printf("Error sending Slack response: %v", err)
		return
	}

	// Save to conversation history for synthesis (memory summarization)
	h.conversationsMu.Lock()
	messages := h.conversations[event.Channel]
	messages = append(messages, conversationMessage{
		Role:    "user",
		Content: cleanedMessage,
	})
	messages = append(messages, conversationMessage{
		Role:    "assistant",
		Content: cleanResponse,
	})
	// Keep conversation window (last 20 messages)
	if len(messages) > 20 {
		messages = messages[len(messages)-20:]
	}
	h.conversations[event.Channel] = messages
	h.conversationsMu.Unlock()
}

func (h *Handler) processWithClaude(ctx context.Context, agentName string, systemPrompt string, messages []conversationMessage) (string, error) {
	// Filter out any messages with empty content to prevent API errors
	filteredMessages := make([]conversationMessage, 0, len(messages))
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content != "" {
			filteredMessages = append(filteredMessages, conversationMessage{
				Role:    msg.Role,
				Content: content,
			})
		}
	}
	messages = filteredMessages

	if len(messages) == 0 {
		return "", fmt.Errorf("no valid messages to process")
	}

	// Get agent definition
	agentDef, ok := h.config.Agents[agentName]
	if !ok {
		return "", fmt.Errorf("%s agent not found", agentName)
	}

	agent := vega.Agent{
		Name:   agentDef.Name,
		Model:  agentDef.Model,
		System: vega.StaticPrompt(systemPrompt),
		Tools:  h.tools,
	}

	if agentDef.Temperature != nil {
		agent.Temperature = agentDef.Temperature
	}

	// Spawn process
	proc, err := h.orch.Spawn(agent, vega.WithTask("Slack conversation"))
	if err != nil {
		return "", fmt.Errorf("failed to spawn process: %w", err)
	}

	// Get the last user message
	var lastUserMessage string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUserMessage = messages[i].Content
			break
		}
	}

	if lastUserMessage == "" {
		return "", fmt.Errorf("no user message found in conversation")
	}

	response, err := proc.Send(ctx, lastUserMessage)
	if err != nil {
		proc.Fail(err)
		return "", fmt.Errorf("failed to get response: %w", err)
	}

	// Mark process as completed
	proc.Complete(response)

	return response, nil
}

func (h *Handler) getUser(userID string) *User {
	// Check cache
	h.userCacheMu.RLock()
	user, ok := h.userCache[userID]
	h.userCacheMu.RUnlock()

	if ok {
		return user
	}

	// Fetch from API
	user, err := h.client.GetUserInfo(userID)
	if err != nil {
		log.Printf("Error getting user info for %s: %v", userID, err)
		return nil
	}

	// Cache
	h.userCacheMu.Lock()
	h.userCache[userID] = user
	h.userCacheMu.Unlock()

	return user
}

// getChannelName returns the channel name for a channel ID
func (h *Handler) getChannelName(channelID string) string {
	// Check cache
	h.channelCacheMu.RLock()
	name, ok := h.channelCache[channelID]
	h.channelCacheMu.RUnlock()

	if ok {
		return name
	}

	// Fetch from API
	name, err := h.client.GetChannelName(channelID)
	if err != nil {
		log.Printf("Error getting channel name for %s: %v", channelID, err)
		return ""
	}

	// Cache
	h.channelCacheMu.Lock()
	h.channelCache[channelID] = name
	h.channelCacheMu.Unlock()

	return name
}

// parsePersonaPrefix extracts persona name and cleaned message from text
// Returns (personaName, cleanedMessage) - personaName is empty if no prefix found
// Supports: "tony: message", "Tony: message", "tony, message", etc.
func parsePersonaPrefix(text string) (string, string) {
	match := personaPrefixPattern.FindStringSubmatch(text)
	if match == nil {
		return "", text
	}

	// Capitalize first letter to match agent names in config
	persona := strings.ToUpper(match[1][:1]) + strings.ToLower(match[1][1:])
	cleaned := personaPrefixPattern.ReplaceAllString(text, "")

	return persona, cleaned
}

// routeWithTron asks Tron to decide which persona should handle the message
func (h *Handler) routeWithTron(ctx context.Context, message string) string {
	defaultAgent := "Tony"

	// Get a model to use (borrow from any agent config)
	var model string
	for _, agentDef := range h.config.Agents {
		model = agentDef.Model
		break
	}
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}

	agent := vega.Agent{
		Name:   "Tron",
		Model:  model,
		System: vega.StaticPrompt(tronRouterPrompt),
	}

	proc, err := h.orch.Spawn(agent, vega.WithTask("Routing message"))
	if err != nil {
		log.Printf("Failed to spawn Tron router: %v", err)
		return defaultAgent
	}

	response, err := proc.Send(ctx, message)
	if err != nil {
		proc.Fail(err)
		log.Printf("Failed to get routing decision from Tron: %v", err)
		return defaultAgent
	}

	// Mark router as completed
	proc.Complete(response)

	// Extract persona name from response
	match := routerResponsePattern.FindStringSubmatch(response)
	if match == nil {
		log.Printf("Tron routing response didn't match expected pattern: %q, defaulting to %s", response, defaultAgent)
		return defaultAgent
	}

	persona := strings.ToUpper(match[1][:1]) + strings.ToLower(match[1][1:])

	// Verify agent exists
	if _, ok := h.config.Agents[persona]; !ok {
		log.Printf("Tron routed to unknown agent %q, defaulting to %s", persona, defaultAgent)
		return defaultAgent
	}

	log.Printf("Tron routed message to %s", persona)
	return persona
}

// resolveAgentFromMessage determines which agent to use based on message prefix and channel
// Priority: 1) Dedicated persona handler, 2) Message prefix (tony: ...), 3) Channel-specific (#tron-maya), 4) Tron router
// Returns (agentName, cleanedMessage)
func (h *Handler) resolveAgentFromMessage(ctx context.Context, channelName, messageText string) (string, string) {
	// If this handler is dedicated to a specific persona, use it directly
	if h.persona != "" {
		if _, ok := h.config.Agents[h.persona]; ok {
			return h.persona, messageText
		}
		log.Printf("Dedicated persona %q not found in config, falling back", h.persona)
	}

	// First, check for persona prefix in message (works in any tron channel)
	if strings.HasPrefix(channelName, "tron") {
		persona, cleanedText := parsePersonaPrefix(messageText)
		if persona != "" {
			// Verify agent exists in config
			if _, ok := h.config.Agents[persona]; ok {
				return persona, cleanedText
			}
			log.Printf("Agent %q from message prefix not found in config", persona)
		}
	}

	// Fall back to channel-based routing for #tron-{agent} channels
	if strings.HasPrefix(channelName, "tron-") {
		agentName := strings.TrimPrefix(channelName, "tron-")
		// Capitalize first letter to match agent names in config
		if len(agentName) > 0 {
			agentName = strings.ToUpper(agentName[:1]) + strings.ToLower(agentName[1:])
		}
		// Verify agent exists in config
		if _, ok := h.config.Agents[agentName]; ok {
			return agentName, messageText
		}
		log.Printf("Agent %q not found in config", agentName)
	}

	// For #tron channel with no prefix, use Tron to route
	if channelName == "tron" {
		persona := h.routeWithTron(ctx, messageText)
		return persona, messageText
	}

	// Ultimate fallback
	return "Tony", messageText
}

func (h *Handler) synthesisLoop() {
	defer h.wg.Done()

	ticker := time.NewTicker(synthesisCheckPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.checkForSynthesis()
		}
	}
}

func (h *Handler) checkForSynthesis() {
	h.activityMu.RLock()
	channels := make(map[string]time.Time)
	for ch, lastActive := range h.lastActivity {
		channels[ch] = lastActive
	}
	h.activityMu.RUnlock()

	now := time.Now()
	for channel, lastActive := range channels {
		// Check if channel has been inactive long enough
		if now.Sub(lastActive) < slackSynthesisDelay {
			continue
		}

		// Check if we've already synthesized recently
		h.activityMu.RLock()
		lastSynth := h.lastSynthesis[channel]
		h.activityMu.RUnlock()

		if !lastSynth.IsZero() && lastSynth.After(lastActive) {
			continue
		}

		// Synthesize
		h.synthesizeConversation(channel)

		// Update synthesis time
		h.activityMu.Lock()
		h.lastSynthesis[channel] = now
		h.activityMu.Unlock()
	}
}

func (h *Handler) synthesizeConversation(channel string) {
	h.conversationsMu.RLock()
	messages, ok := h.conversations[channel]
	h.conversationsMu.RUnlock()

	if !ok || len(messages) == 0 {
		return
	}

	// Build conversation text
	var sb strings.Builder
	for _, msg := range messages {
		sb.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
	}

	// Get summary from Claude
	ctx := context.Background()
	tonyDef, ok := h.config.Agents["Tony"]
	if !ok {
		return
	}

	agent := vega.Agent{
		Name:   "Summarizer",
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(memory.SummarizePrompt()),
	}

	proc, err := h.orch.Spawn(agent, vega.WithTask("Summarizing conversation"))
	if err != nil {
		log.Printf("Failed to spawn summarizer: %v", err)
		return
	}

	summary, err := proc.Send(ctx, sb.String())
	if err != nil {
		proc.Fail(err)
		log.Printf("Failed to get summary: %v", err)
		return
	}

	// Mark summarizer as completed
	proc.Complete(summary)

	// Append to memory
	if err := memory.Append(h.baseDir, "Slack conversation", summary); err != nil {
		log.Printf("Failed to append memory: %v", err)
	}

	// Clear conversation
	h.conversationsMu.Lock()
	delete(h.conversations, channel)
	h.conversationsMu.Unlock()
}

func (h *Handler) eventCacheCleanup() {
	defer h.wg.Done()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.cleanupEventCache()
		}
	}
}

func (h *Handler) cleanupEventCache() {
	h.processedEventsMu.Lock()
	defer h.processedEventsMu.Unlock()

	cutoff := time.Now().Add(-eventCacheTTL)
	for eventID, timestamp := range h.processedEvents {
		if timestamp.Before(cutoff) {
			delete(h.processedEvents, eventID)
		}
	}

	// Also cleanup stale sessions (completed or failed processes)
	h.cleanupStaleSessions()
}

// cleanupStaleSessions removes sessions with completed or failed processes
func (h *Handler) cleanupStaleSessions() {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()

	for channel, proc := range h.sessions {
		status := proc.Status()
		if status == vega.StatusCompleted || status == vega.StatusFailed {
			log.Printf("[slack] Cleaning up stale session for channel %s (status: %v)", channel, status)
			delete(h.sessions, channel)
		}
	}
}

// stripToolCalls removes XML-style tool call and result blocks from response text
// so raw tool markup doesn't get shown to users in Slack
func stripToolCalls(response string) string {
	cleaned := xmlTagPattern.ReplaceAllString(response, "")
	// Clean up excessive newlines left behind after stripping
	multiNewline := regexp.MustCompile(`\n{3,}`)
	cleaned = multiNewline.ReplaceAllString(cleaned, "\n\n")
	return strings.TrimSpace(cleaned)
}
