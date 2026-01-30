package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/martellcode/tron/internal/memory"
	"github.com/martellcode/vega"
)

const (
	debounceWindow = 250 * time.Millisecond
)

// debounceSession tracks pending requests per call
type debounceSession struct {
	callID        string
	callerPhone   string
	callerName    string
	lastMessage   string
	timer         *time.Timer
	responseReady chan struct{}
	pendingReq    *ChatCompletionRequest
	mu            sync.Mutex
}

// activeRequest prevents concurrent processing per callID
type activeRequest struct {
	cancel func()
	done   chan struct{}
}

// VAPI debouncing state
type vapiState struct {
	debounceSessions map[string]*debounceSession
	activeRequests   map[string]*activeRequest
	callInfo         map[string]callInfoEntry // callID -> phone info
	mu               sync.RWMutex
}

type callInfoEntry struct {
	phone     string
	name      string
	timestamp time.Time
}

func newVAPIState() *vapiState {
	return &vapiState{
		debounceSessions: make(map[string]*debounceSession),
		activeRequests:   make(map[string]*activeRequest),
		callInfo:         make(map[string]callInfoEntry),
	}
}

// handleVAPIEvents handles VAPI webhook events
func (s *Server) handleVAPIEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var event map[string]interface{}
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract event type
	eventType, _ := event["type"].(string)

	switch eventType {
	case "call.started":
		s.cacheCallInfo(event)
	case "call.end", "end-of-call-report":
		s.handleCallEnd(event)
	}

	w.WriteHeader(http.StatusOK)
}

// cacheCallInfo extracts and caches caller info from VAPI event
func (s *Server) cacheCallInfo(event map[string]interface{}) {
	callID, _ := event["call_id"].(string)
	if callID == "" {
		if call, ok := event["call"].(map[string]interface{}); ok {
			callID, _ = call["id"].(string)
		}
	}

	if callID == "" {
		return
	}

	// Extract customer phone
	var phone, name string
	if customer, ok := event["customer"].(map[string]interface{}); ok {
		phone, _ = customer["number"].(string)
		name, _ = customer["name"].(string)
	}

	if phone == "" {
		return
	}

	s.vapiState.mu.Lock()
	s.vapiState.callInfo[callID] = callInfoEntry{
		phone:     phone,
		name:      name,
		timestamp: time.Now(),
	}
	s.vapiState.mu.Unlock()

	log.Printf("Cached call info for %s: phone=%s", callID, maskPhone(phone))
}

// handleCallEnd processes end-of-call events for memory synthesis
func (s *Server) handleCallEnd(event map[string]interface{}) {
	callID, _ := event["call_id"].(string)
	if callID == "" {
		if call, ok := event["call"].(map[string]interface{}); ok {
			callID, _ = call["id"].(string)
		}
	}

	// Get caller info
	s.vapiState.mu.RLock()
	info, ok := s.vapiState.callInfo[callID]
	s.vapiState.mu.RUnlock()

	callerName := "Unknown Caller"
	if ok && info.name != "" {
		callerName = info.name
	} else if ok && info.phone != "" {
		// Try to identify from contacts
		callerInfo := s.customTools.IdentifyCaller(info.phone)
		if callerInfo != "" {
			// Extract name from caller info
			lines := strings.Split(callerInfo, "\n")
			if len(lines) > 0 {
				callerName = strings.TrimPrefix(lines[0], "Name: ")
			}
		}
	}

	// Extract transcript from event
	var transcript string
	if messages, ok := event["messages"].([]interface{}); ok {
		var sb strings.Builder
		for _, m := range messages {
			if msg, ok := m.(map[string]interface{}); ok {
				role, _ := msg["role"].(string)
				content, _ := msg["content"].(string)
				sb.WriteString(role + ": " + content + "\n")
			}
		}
		transcript = sb.String()
	}

	if transcript == "" {
		log.Printf("No transcript in call end event for %s", callID)
		return
	}

	// Synthesize memory
	go s.synthesizeCallMemory(callerName, transcript)

	// Clean up call info
	s.vapiState.mu.Lock()
	delete(s.vapiState.callInfo, callID)
	s.vapiState.mu.Unlock()
}

func (s *Server) synthesizeCallMemory(callerName, transcript string) {
	// Get Tony for summarization
	tonyDef, ok := s.config.Agents["Tony"]
	if !ok {
		return
	}

	agent := vega.Agent{
		Name:   "Summarizer",
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(memory.SummarizePrompt()),
	}

	proc, err := s.orch.Spawn(agent)
	if err != nil {
		log.Printf("Failed to spawn summarizer: %v", err)
		return
	}

	summary, err := proc.Send(nil, transcript)
	if err != nil {
		log.Printf("Failed to summarize call: %v", err)
		return
	}

	if err := memory.Append(s.baseDir, callerName, summary); err != nil {
		log.Printf("Failed to save memory: %v", err)
	}
}

// getCallerFromVAPI looks up caller info from VAPI cache
func (s *Server) getCallerFromVAPI(callID string) (string, string) {
	s.vapiState.mu.RLock()
	info, ok := s.vapiState.callInfo[callID]
	s.vapiState.mu.RUnlock()

	if ok {
		return info.phone, info.name
	}
	return "", ""
}

// cleanupVAPICache removes stale call info entries
func (s *Server) cleanupVAPICache(maxAge time.Duration) {
	s.vapiState.mu.Lock()
	defer s.vapiState.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	for callID, info := range s.vapiState.callInfo {
		if info.timestamp.Before(cutoff) {
			delete(s.vapiState.callInfo, callID)
		}
	}
}

func maskPhone(phone string) string {
	if len(phone) <= 6 {
		return "***"
	}
	return phone[:3] + "***" + phone[len(phone)-4:]
}
