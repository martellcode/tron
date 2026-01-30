package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/martellcode/tron/internal/memory"
	"github.com/martellcode/tron/internal/voice/elevenlabs"
	"github.com/martellcode/vega"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

// ElevenLabsSession bridges a web client to ElevenLabs
type ElevenLabsSession struct {
	ClientConn     *websocket.Conn
	ElevenLabsConn *elevenlabs.Session
	ConversationID string
	StartTime      time.Time
	mu             sync.Mutex
}

// handleElevenLabsWS handles WebSocket connections for ElevenLabs voice
func (s *Server) handleElevenLabsWS(w http.ResponseWriter, r *http.Request) {
	if s.elevenLabsClient == nil || !s.elevenLabsClient.IsConfigured() {
		http.Error(w, "ElevenLabs not configured", http.StatusServiceUnavailable)
		return
	}

	// Upgrade to WebSocket
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer clientConn.Close()

	// Connect to ElevenLabs
	ctx := r.Context()
	elSession, err := s.elevenLabsClient.Connect(ctx)
	if err != nil {
		log.Printf("Failed to connect to ElevenLabs: %v", err)
		clientConn.WriteJSON(map[string]string{
			"type":  "error",
			"error": "Failed to connect to ElevenLabs",
		})
		return
	}
	defer elSession.Close()

	session := &ElevenLabsSession{
		ClientConn:     clientConn,
		ElevenLabsConn: elSession,
		ConversationID: elSession.ConversationID(),
		StartTime:      time.Now(),
	}

	// Send session info to client
	clientConn.WriteJSON(map[string]string{
		"type":       "session_started",
		"session_id": session.ConversationID,
	})

	// Start forwarding goroutines
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		s.forwardClientToElevenLabs(session)
	}()

	go func() {
		defer wg.Done()
		s.forwardElevenLabsAudioToClient(session)
	}()

	go func() {
		defer wg.Done()
		s.forwardTranscriptsToClient(session)
	}()

	wg.Wait()
}

func (s *Server) forwardClientToElevenLabs(session *ElevenLabsSession) {
	for {
		select {
		case <-session.ElevenLabsConn.Done():
			return
		default:
		}

		_, message, err := session.ClientConn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("Client WebSocket error: %v", err)
			}
			session.ElevenLabsConn.Close()
			return
		}

		// Parse client message
		var msg struct {
			Type  string `json:"type"`
			Audio string `json:"audio"` // base64 encoded
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "audio":
			// Decode base64 audio and forward
			audio, err := base64.StdEncoding.DecodeString(msg.Audio)
			if err != nil {
				log.Printf("Failed to decode audio: %v", err)
				continue
			}
			if err := session.ElevenLabsConn.SendAudio(audio); err != nil {
				log.Printf("Failed to send audio: %v", err)
			}

		case "text":
			if err := session.ElevenLabsConn.SendText(msg.Text); err != nil {
				log.Printf("Failed to send text: %v", err)
			}

		case "end":
			session.ElevenLabsConn.Close()
			return
		}
	}
}

func (s *Server) forwardElevenLabsAudioToClient(session *ElevenLabsSession) {
	for audio := range session.ElevenLabsConn.Audio() {
		// Base64 encode and send to client
		encoded := base64.StdEncoding.EncodeToString(audio)

		session.mu.Lock()
		err := session.ClientConn.WriteJSON(map[string]string{
			"type":  "audio",
			"audio": encoded,
		})
		session.mu.Unlock()

		if err != nil {
			log.Printf("Failed to send audio to client: %v", err)
			return
		}
	}
}

func (s *Server) forwardTranscriptsToClient(session *ElevenLabsSession) {
	for transcript := range session.ElevenLabsConn.Transcripts() {
		session.mu.Lock()
		err := session.ClientConn.WriteJSON(map[string]interface{}{
			"type":     "transcript",
			"role":     transcript.Role,
			"text":     transcript.Text,
			"is_final": transcript.IsFinal,
		})
		session.mu.Unlock()

		if err != nil {
			log.Printf("Failed to send transcript to client: %v", err)
			return
		}
	}
}

// OpenAI-compatible types for ElevenLabs LLM endpoint
type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      *openAIMessage `json:"message,omitempty"`
	Delta        *openAIDelta  `json:"delta,omitempty"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}

type openAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// handleElevenLabsLLM handles the LLM endpoint for ElevenLabs conversational AI
func (s *Server) handleElevenLabsLLM(w http.ResponseWriter, r *http.Request) {
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

	var req openAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract conversation context from headers or request
	conversationID := r.Header.Get("X-Conversation-ID")
	userID := r.Header.Get("X-User-ID")

	// Generate stable conversation ID from message hash if not provided
	if conversationID == "" && len(req.Messages) > 0 {
		hash := sha256.Sum256([]byte(req.Messages[0].Content))
		conversationID = hex.EncodeToString(hash[:8])
	}

	// Get Tony definition
	tonyDef, ok := s.config.Agents["Tony"]
	if !ok {
		http.Error(w, "Tony agent not found", http.StatusInternalServerError)
		return
	}

	// Build system prompt with voice context
	systemPrompt := tonyDef.System
	systemPrompt += "\n\n## Voice Conversation Context"
	systemPrompt += "\nYou are in a real-time voice conversation. Keep responses concise and natural."
	systemPrompt += "\nSpeak in short sentences suitable for text-to-speech."

	// Add caller context if available
	if userID != "" {
		callerInfo := s.customTools.IdentifyCaller(userID)
		if callerInfo != "" {
			systemPrompt += fmt.Sprintf("\n\n## Current Caller\n%s", callerInfo)
		}
	}

	// Load memory
	memContent, _ := memory.Load(s.baseDir)
	if memContent != "" {
		systemPrompt += memory.GetPromptSection(memContent)
	}

	// Extract user message
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
		s.handleElevenLabsStreamingResponse(w, r.Context(), systemPrompt, userMessage, conversationID)
	} else {
		s.handleElevenLabsNonStreamingResponse(w, r.Context(), systemPrompt, userMessage, conversationID)
	}
}

func (s *Server) handleElevenLabsStreamingResponse(w http.ResponseWriter, ctx context.Context, systemPrompt, message, conversationID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// Get Tony definition
	tonyDef, ok := s.config.Agents["Tony"]
	if !ok {
		return
	}

	// Build agent
	vegaTools := vega.NewTools(vega.WithSandbox(s.workingDir))
	vegaTools.RegisterBuiltins()
	s.customTools.RegisterTo(vegaTools)

	agent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(systemPrompt),
		Tools:  vegaTools,
	}

	if tonyDef.Temperature != nil {
		agent.Temperature = tonyDef.Temperature
	}

	// Spawn process
	proc, err := s.orch.Spawn(agent)
	if err != nil {
		s.writeSSEError(w, flusher, err)
		return
	}

	stream, err := proc.SendStream(ctx, message)
	if err != nil {
		s.writeSSEError(w, flusher, err)
		return
	}

	responseID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	// Send initial chunk
	initialChunk := openAIChatResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []openAIChoice{{
			Index: 0,
			Delta: &openAIDelta{Role: "assistant"},
		}},
	}
	s.writeOpenAISSE(w, flusher, initialChunk)

	// Sentence buffering for TTS
	var buffer strings.Builder
	sentenceEnders := []string{".", "!", "?", "\n"}
	firstSentenceSent := false
	flushTimeout := time.NewTimer(150 * time.Millisecond)

	flushBuffer := func() {
		if buffer.Len() > 0 {
			chunk := openAIChatResponse{
				ID:      responseID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   "tony",
				Choices: []openAIChoice{{
					Index: 0,
					Delta: &openAIDelta{Content: buffer.String()},
				}},
			}
			s.writeOpenAISSE(w, flusher, chunk)
			buffer.Reset()
			firstSentenceSent = true
		}
	}

	for {
		select {
		case chunk, ok := <-stream.Chunks():
			if !ok {
				flushBuffer()
				goto done
			}

			buffer.WriteString(chunk)

			// Check for sentence end
			endsWithSentence := false
			for _, ender := range sentenceEnders {
				if strings.HasSuffix(strings.TrimSpace(buffer.String()), ender) {
					endsWithSentence = true
					break
				}
			}

			// Flush on sentence end, or immediately for first sentence
			if endsWithSentence || (!firstSentenceSent && buffer.Len() > 50) {
				flushBuffer()
				flushTimeout.Reset(150 * time.Millisecond)
			}

		case <-flushTimeout.C:
			flushBuffer()
			flushTimeout.Reset(150 * time.Millisecond)
		}
	}

done:
	flushTimeout.Stop()

	// Send final chunk
	finishReason := "stop"
	finalChunk := openAIChatResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []openAIChoice{{
			Index:        0,
			Delta:        &openAIDelta{},
			FinishReason: &finishReason,
		}},
	}
	s.writeOpenAISSE(w, flusher, finalChunk)

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) handleElevenLabsNonStreamingResponse(w http.ResponseWriter, ctx context.Context, systemPrompt, message, conversationID string) {
	// Get Tony definition
	tonyDef, ok := s.config.Agents["Tony"]
	if !ok {
		http.Error(w, "Tony agent not found", http.StatusInternalServerError)
		return
	}

	// Build agent
	vegaTools := vega.NewTools(vega.WithSandbox(s.workingDir))
	vegaTools.RegisterBuiltins()
	s.customTools.RegisterTo(vegaTools)

	agent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(systemPrompt),
		Tools:  vegaTools,
	}

	if tonyDef.Temperature != nil {
		agent.Temperature = tonyDef.Temperature
	}

	// Spawn process
	proc, err := s.orch.Spawn(agent)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to spawn process: %v", err), http.StatusInternalServerError)
		return
	}

	response, err := proc.Send(ctx, message)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get response: %v", err), http.StatusInternalServerError)
		return
	}

	finishReason := "stop"
	chatResponse := openAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []openAIChoice{{
			Index: 0,
			Message: &openAIMessage{
				Role:    "assistant",
				Content: response,
			},
			FinishReason: &finishReason,
		}},
		Usage: &openAIUsage{
			PromptTokens:     100, // Estimate
			CompletionTokens: len(response) / 4,
			TotalTokens:      100 + len(response)/4,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResponse)
}

func (s *Server) writeOpenAISSE(w http.ResponseWriter, flusher http.Flusher, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()
}
