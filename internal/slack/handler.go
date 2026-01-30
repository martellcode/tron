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

	"github.com/martellcode/tron/internal/memory"
	"github.com/martellcode/vega"
	"github.com/martellcode/vega/dsl"
)

// xmlTagPattern matches XML-style tags used for tool calls and results
var xmlTagPattern = regexp.MustCompile(`(?s)<[a-z_:]+[^>]*>.*?</[a-z_:]+>`)

const (
	maxToolLoops            = 10
	slackSynthesisDelay     = 30 * time.Minute
	synthesisCheckPeriod    = 5 * time.Minute
	eventCacheTTL           = 1 * time.Hour
	timestampValidityWindow = 5 * time.Minute
)

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

	// Tool execution
	tools       *vega.Tools
	customTools interface{} // *tools.PersonaTools

	// Lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewHandler creates a new Slack event handler
func NewHandler(client *Client, signingSecret string, orch *vega.Orchestrator, config *dsl.Document, baseDir string) *Handler {
	h := &Handler{
		client:          client,
		signingSecret:   signingSecret,
		orch:            orch,
		config:          config,
		baseDir:         baseDir,
		conversations:   make(map[string][]conversationMessage),
		lastActivity:    make(map[string]time.Time),
		lastSynthesis:   make(map[string]time.Time),
		userCache:       make(map[string]*User),
		channelCache:    make(map[string]string),
		processedEvents: make(map[string]time.Time),
		stopCh:          make(chan struct{}),
	}

	// Start synthesis loop
	h.wg.Add(1)
	go h.synthesisLoop()

	// Start event cache cleanup
	h.wg.Add(1)
	go h.eventCacheCleanup()

	return h
}

// SetTools sets the tools for the handler
func (h *Handler) SetTools(tools *vega.Tools) {
	h.tools = tools
}

// SetCustomTools sets the custom persona tools
func (h *Handler) SetCustomTools(customTools interface{}) {
	h.customTools = customTools
}

// Shutdown gracefully stops the handler
func (h *Handler) Shutdown() {
	close(h.stopCh)
	h.wg.Wait()
}

// HandleEvents is the HTTP handler for Slack events
func (h *Handler) HandleEvents(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, "Invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Parse event
	var payload EventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

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

	// Update activity
	h.activityMu.Lock()
	h.lastActivity[event.Channel] = time.Now()
	h.activityMu.Unlock()

	// Get user info
	user := h.getUser(event.User)
	userName := "Unknown"
	if user != nil {
		userName = user.RealName
		if userName == "" {
			userName = user.Name
		}
	}

	// Load conversation
	h.conversationsMu.Lock()
	messages, ok := h.conversations[event.Channel]
	if !ok {
		messages = []conversationMessage{}
	}
	h.conversationsMu.Unlock()

	// Resolve agent based on channel name
	channelName := h.getChannelName(event.Channel)
	agentName := h.resolveAgentFromChannel(channelName)

	// Build system prompt
	agentDef, ok := h.config.Agents[agentName]
	if !ok {
		log.Printf("%s agent not found in config", agentName)
		return
	}

	systemPrompt := agentDef.System
	systemPrompt += fmt.Sprintf("\n\n## Current Context\nChannel: Slack\nUser: %s\n", userName)

	// Load memory
	memContent, _ := memory.Load(h.baseDir)
	if memContent != "" {
		systemPrompt += memory.GetPromptSection(memContent)
	}

	// Add user message
	messages = append(messages, conversationMessage{
		Role:    "user",
		Content: event.Text,
	})

	// Keep conversation window (last 20 messages)
	if len(messages) > 20 {
		messages = messages[len(messages)-20:]
	}

	// Process with Claude
	ctx := context.Background()
	response, err := h.processWithClaude(ctx, agentName, systemPrompt, messages)
	if err != nil {
		log.Printf("Error processing Slack message: %v", err)
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

	// Save to conversation history (use cleaned response)
	messages = append(messages, conversationMessage{
		Role:    "assistant",
		Content: cleanResponse,
	})

	h.conversationsMu.Lock()
	h.conversations[event.Channel] = messages
	h.conversationsMu.Unlock()
}

func (h *Handler) processWithClaude(ctx context.Context, agentName string, systemPrompt string, messages []conversationMessage) (string, error) {
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
	proc, err := h.orch.Spawn(agent)
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

	response, err := proc.Send(ctx, lastUserMessage)
	if err != nil {
		return "", fmt.Errorf("failed to get response: %w", err)
	}

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

// resolveAgentFromChannel determines which agent to use based on channel name
// Supports: #tron (default to Tony), #tron-tony, #tron-maya, etc.
func (h *Handler) resolveAgentFromChannel(channelName string) string {
	// Default agent
	defaultAgent := "Tony"

	// Check for #tron-{agent} pattern
	if strings.HasPrefix(channelName, "tron-") {
		agentName := strings.TrimPrefix(channelName, "tron-")
		// Capitalize first letter to match agent names in config
		if len(agentName) > 0 {
			agentName = strings.ToUpper(agentName[:1]) + strings.ToLower(agentName[1:])
		}
		// Verify agent exists in config
		if _, ok := h.config.Agents[agentName]; ok {
			return agentName
		}
		log.Printf("Agent %q not found in config, falling back to %s", agentName, defaultAgent)
	}

	return defaultAgent
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

	proc, err := h.orch.Spawn(agent)
	if err != nil {
		log.Printf("Failed to spawn summarizer: %v", err)
		return
	}

	summary, err := proc.Send(ctx, sb.String())
	if err != nil {
		log.Printf("Failed to get summary: %v", err)
		return
	}

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
