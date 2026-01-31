package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/martellcode/tron/internal/callback"
	"github.com/martellcode/tron/internal/slack"
	"github.com/martellcode/tron/internal/subdomain"
	"github.com/martellcode/tron/internal/tools"
	"github.com/martellcode/tron/internal/voice/elevenlabs"
	"github.com/martellcode/vega"
	"github.com/martellcode/vega/dsl"
)

// Server handles VAPI webhooks and provides OpenAI-compatible chat completions
type Server struct {
	orch        *vega.Orchestrator
	config      *dsl.Document
	customTools *tools.PersonaTools
	port        int
	workingDir  string
	baseDir     string
	httpServer  *http.Server

	// Session management - maps caller ID to their Tony process
	sessions   map[string]*vega.Process
	sessionsMu sync.RWMutex

	// VAPI state for debouncing and call tracking
	vapiState *vapiState

	// ElevenLabs client for voice
	elevenLabsClient *elevenlabs.Client

	// Slack handler
	slackHandler *slack.Handler

	// Callback registry
	callbackRegistry *callback.Registry

	// Subdomain routing for project servers
	subdomainRegistry *subdomain.Registry
	processManager    *subdomain.ProcessManager

	// Life loop for triggering activities
	lifeLoop LifeLoop
}

// LifeLoop interface for Tony's autonomous routine (to avoid circular imports)
type LifeLoop interface {
	TriggerActivity(activity string) string
}

// New creates a new server instance
func New(orch *vega.Orchestrator, config *dsl.Document, customTools *tools.PersonaTools, port int, workingDir string) *Server {
	// Initialize subdomain routing
	subdomainReg := subdomain.NewRegistry()
	procManager := subdomain.NewProcessManager(subdomainReg)

	s := &Server{
		orch:              orch,
		config:            config,
		customTools:       customTools,
		port:              port,
		workingDir:        workingDir,
		baseDir:           ".", // Default to current directory
		sessions:          make(map[string]*vega.Process),
		vapiState:         newVAPIState(),
		subdomainRegistry: subdomainReg,
		processManager:    procManager,
	}

	mux := http.NewServeMux()

	// Chat completion endpoints (VAPI compatible)
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)

	// VAPI events webhook
	mux.HandleFunc("/vapi/events", s.handleVAPIEvents)

	// ElevenLabs endpoints
	mux.HandleFunc("/ws/elevenlabs", s.handleElevenLabsWS)
	mux.HandleFunc("/v1/elevenlabs-llm", s.handleElevenLabsLLM)

	// Slack events webhook (registered if handler is set)
	mux.HandleFunc("/slack/events", s.handleSlackEvents)

	// Caddy on-demand TLS verification endpoint
	mux.HandleFunc("/internal/caddy-ask", s.subdomainRegistry.HandleCaddyAsk)

	// Health check
	mux.HandleFunc("/health", s.handleHealth)

	// Life loop trigger endpoint (for testing/demos)
	mux.HandleFunc("/internal/life/trigger", s.handleLifeTrigger)

	// Wrap with subdomain routing middleware
	handler := s.subdomainRegistry.Middleware(mux)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming
	}

	return s
}

// SetBaseDir sets the base directory for memory and config files
func (s *Server) SetBaseDir(dir string) {
	s.baseDir = dir
}

// SetElevenLabsClient sets the ElevenLabs client
func (s *Server) SetElevenLabsClient(client *elevenlabs.Client) {
	s.elevenLabsClient = client
}

// SetSlackHandler sets the Slack event handler
func (s *Server) SetSlackHandler(handler *slack.Handler) {
	s.slackHandler = handler
}

// SetCallbackRegistry sets the callback registry
func (s *Server) SetCallbackRegistry(registry *callback.Registry) {
	s.callbackRegistry = registry
}

// GetProcessManager returns the process manager for starting project servers
func (s *Server) GetProcessManager() *subdomain.ProcessManager {
	return s.processManager
}

// GetSubdomainRegistry returns the subdomain registry
func (s *Server) GetSubdomainRegistry() *subdomain.Registry {
	return s.subdomainRegistry
}

// SetLifeLoop sets the life loop for triggering activities
func (s *Server) SetLifeLoop(loop LifeLoop) {
	s.lifeLoop = loop
}

// ListenAndServe starts the HTTP server
func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	if s.slackHandler != nil {
		s.slackHandler.Shutdown()
	}
	if s.processManager != nil {
		s.processManager.Shutdown()
	}
	return s.httpServer.Shutdown(ctx)
}

// handleSlackEvents delegates to the Slack handler if configured
func (s *Server) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	if s.slackHandler == nil {
		http.Error(w, "Slack not configured", http.StatusServiceUnavailable)
		return
	}
	s.slackHandler.HandleEvents(w, r)
}

// OpenAI-compatible request/response structures
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatDelta   `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason,omitempty"`
}

type ChatDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleLifeTrigger(w http.ResponseWriter, r *http.Request) {
	if s.lifeLoop == nil {
		http.Error(w, "Life loop not configured", http.StatusServiceUnavailable)
		return
	}

	activity := r.URL.Query().Get("activity")
	if activity == "" {
		// Return list of valid activities
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      "Missing 'activity' query parameter",
			"activities": []string{"news", "goals", "team_check", "reflection", "journal", "post"},
			"example":    "/internal/life/trigger?activity=news",
		})
		return
	}

	result := s.lifeLoop.TriggerActivity(activity)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"activity": activity,
		"result":   result,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Log incoming request for debugging
	log.Printf("[chat] Incoming request from %s, body length: %d", r.RemoteAddr, len(body))
	log.Printf("[chat] Request body: %s", string(body))

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[chat] JSON parse error: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract caller info from VAPI headers
	callerPhone := r.Header.Get("X-Vapi-Caller-Phone")
	callID := r.Header.Get("X-Vapi-Call-ID")

	// Try to get phone from VAPI cache if not in header
	if callerPhone == "" && callID != "" {
		callerPhone, _ = s.getCallerFromVAPI(callID)
	}

	callerID := callerPhone
	if callerID == "" {
		callerID = r.Header.Get("X-Request-ID")
		if callerID == "" {
			callerID = fmt.Sprintf("anonymous-%d", time.Now().UnixNano())
		}
	}

	// Get or create session for this caller
	proc, err := s.getOrCreateSession(r.Context(), callerID, callerPhone)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create session: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract the user message (last message with role "user")
	var userMessage string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userMessage = req.Messages[i].Content
			break
		}
	}

	if userMessage == "" {
		log.Printf("[chat] No user message found in %d messages", len(req.Messages))
		http.Error(w, "No user message found", http.StatusBadRequest)
		return
	}

	log.Printf("[chat] Processing message for caller %s: %q (stream=%v)", callerID, userMessage, req.Stream)

	if req.Stream {
		s.handleStreamingResponse(w, r.Context(), proc, userMessage)
	} else {
		s.handleNonStreamingResponse(w, r.Context(), proc, userMessage)
	}
}

func (s *Server) getOrCreateSession(ctx context.Context, callerID, callerPhone string) (*vega.Process, error) {
	s.sessionsMu.RLock()
	proc, exists := s.sessions[callerID]
	s.sessionsMu.RUnlock()

	if exists && proc.Status() == vega.StatusRunning {
		return proc, nil
	}

	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	// Double-check after acquiring write lock
	proc, exists = s.sessions[callerID]
	if exists && proc.Status() == vega.StatusRunning {
		return proc, nil
	}

	// Get Tony agent definition from config
	tonyDef, ok := s.config.Agents["Tony"]
	if !ok {
		return nil, fmt.Errorf("Tony agent not found in config")
	}

	// Build Tony agent
	tonyAgent := s.buildAgent(tonyDef)

	// Add caller context if we have phone number
	if callerPhone != "" {
		// Identify the caller
		callerInfo := s.customTools.IdentifyCaller(callerPhone)
		if callerInfo != "" {
			// Wrap the system prompt with caller context
			originalSystem := tonyDef.System
			tonyAgent.System = vega.StaticPrompt(fmt.Sprintf("%s\n\n## Current Caller\n%s", originalSystem, callerInfo))
		}
	}

	// Spawn new Tony process
	proc, err := s.orch.Spawn(tonyAgent,
		vega.WithSupervision(vega.Supervision{
			Strategy:    vega.Restart,
			MaxRestarts: 3,
			Window:      600_000_000_000, // 10 minutes
		}),
		vega.WithWorkDir(s.workingDir),
	)
	if err != nil {
		return nil, err
	}

	s.sessions[callerID] = proc
	return proc, nil
}

func (s *Server) buildAgent(def *dsl.Agent) vega.Agent {
	vegaTools := vega.NewTools(
		vega.WithSandbox(s.workingDir),
	)
	vegaTools.RegisterBuiltins()
	s.customTools.RegisterTo(vegaTools)

	if len(def.Tools) > 0 {
		vegaTools = vegaTools.Filter(def.Tools...)
	}

	agent := vega.Agent{
		Name:   def.Name,
		Model:  def.Model,
		System: vega.StaticPrompt(def.System),
		Tools:  vegaTools,
	}

	if def.Temperature != nil {
		agent.Temperature = def.Temperature
	}

	if def.Budget != "" {
		agent.Budget = parseBudget(def.Budget)
	}

	return agent
}

// parseBudget converts a budget string like "$5.00" to a Budget struct
func parseBudget(s string) *vega.Budget {
	s = strings.TrimPrefix(s, "$")
	limit, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &vega.Budget{
		Limit:    limit,
		OnExceed: vega.BudgetWarn,
	}
}

func (s *Server) handleStreamingResponse(w http.ResponseWriter, ctx context.Context, proc *vega.Process, message string) {
	log.Printf("[chat] Starting streaming response")

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[chat] Streaming not supported")
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	stream, err := proc.SendStream(ctx, message)
	if err != nil {
		log.Printf("[chat] SendStream error: %v", err)
		s.writeSSEError(w, flusher, err)
		return
	}

	responseID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	// Send initial chunk with role
	initialChunk := ChatCompletionResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []Choice{{
			Index: 0,
			Delta: &ChatDelta{Role: "assistant"},
		}},
	}
	s.writeSSE(w, flusher, initialChunk)

	// Stream content chunks
	var fullResponse strings.Builder
	for chunk := range stream.Chunks() {
		fullResponse.WriteString(chunk)
		chunkResponse := ChatCompletionResponse{
			ID:      responseID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   "tony",
			Choices: []Choice{{
				Index: 0,
				Delta: &ChatDelta{Content: chunk},
			}},
		}
		s.writeSSE(w, flusher, chunkResponse)
	}
	log.Printf("[chat] Full response: %s", fullResponse.String())

	// Send final chunk with finish_reason
	finishReason := "stop"
	finalChunk := ChatCompletionResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []Choice{{
			Index:        0,
			Delta:        &ChatDelta{},
			FinishReason: &finishReason,
		}},
	}
	s.writeSSE(w, flusher, finalChunk)

	// Send [DONE]
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	log.Printf("[chat] Streaming response completed")
}

func (s *Server) writeSSE(w http.ResponseWriter, flusher http.Flusher, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()
}

func (s *Server) writeSSEError(w http.ResponseWriter, flusher http.Flusher, err error) {
	errResponse := map[string]interface{}{
		"error": map[string]string{
			"message": err.Error(),
			"type":    "server_error",
		},
	}
	jsonData, _ := json.Marshal(errResponse)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()
}

func (s *Server) handleNonStreamingResponse(w http.ResponseWriter, ctx context.Context, proc *vega.Process, message string) {
	response, err := proc.Send(ctx, message)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to process message: %v", err), http.StatusInternalServerError)
		return
	}

	finishReason := "stop"
	chatResponse := ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []Choice{{
			Index: 0,
			Message: &ChatMessage{
				Role:    "assistant",
				Content: response,
			},
			FinishReason: &finishReason,
		}},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResponse)
}

// CleanupStaleSessions removes sessions that haven't been used in a while
func (s *Server) CleanupStaleSessions(maxAge time.Duration) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	for id, proc := range s.sessions {
		status := proc.Status()
		if status == vega.StatusCompleted || status == vega.StatusFailed {
			delete(s.sessions, id)
		}
		// Note: Could also check proc.Metrics().LastActivity for age-based cleanup
	}

	// Also cleanup VAPI cache
	s.cleanupVAPICache(maxAge)
}

// Helper to check if a string contains any of the substrings
func containsAny(s string, substrs []string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
