package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/martellcode/tron/internal/tools"
	"github.com/vegaops/vega"
	"github.com/vegaops/vega/dsl"
)

// Server handles VAPI webhooks and provides OpenAI-compatible chat completions
type Server struct {
	orch        *vega.Orchestrator
	config      *dsl.Document
	customTools *tools.PersonaTools
	port        int
	workingDir  string
	httpServer  *http.Server

	// Session management - maps caller ID to their Tony process
	sessions   map[string]*vega.Process
	sessionsMu sync.RWMutex
}

// New creates a new server instance
func New(orch *vega.Orchestrator, config *dsl.Document, customTools *tools.PersonaTools, port int, workingDir string) *Server {
	s := &Server{
		orch:        orch,
		config:      config,
		customTools: customTools,
		port:        port,
		workingDir:  workingDir,
		sessions:    make(map[string]*vega.Process),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions) // OpenAI-compatible path
	mux.HandleFunc("/health", s.handleHealth)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Long timeout for streaming
	}

	return s
}

// ListenAndServe starts the HTTP server
func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
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
	Usage   Usage    `json:"usage,omitempty"`
}

type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message,omitempty"`
	Delta        *ChatDelta  `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason,omitempty"`
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

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract caller info from VAPI headers
	callerPhone := r.Header.Get("X-Vapi-Caller-Phone")
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
		http.Error(w, "No user message found", http.StatusBadRequest)
		return
	}

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
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	stream, err := proc.SendStream(ctx, message)
	if err != nil {
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
	for chunk := range stream.Chunks() {
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
			Message: ChatMessage{
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
