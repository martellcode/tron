package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martellcode/tron/internal/tools"
	"github.com/martellcode/vega"
	"github.com/martellcode/vega/dsl"
)

// mockLLM implements vega.LLM for testing
type mockLLM struct {
	responses []string
	callCount int
}

func (m *mockLLM) Generate(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (*vega.LLMResponse, error) {
	response := "Hello! I'm Tony, your CTO. How can I help?"
	if m.callCount < len(m.responses) {
		response = m.responses[m.callCount]
	}
	m.callCount++
	return &vega.LLMResponse{
		Content:      response,
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.001,
	}, nil
}

func (m *mockLLM) GenerateStream(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (<-chan vega.StreamEvent, error) {
	ch := make(chan vega.StreamEvent, 10)
	go func() {
		defer close(ch)
		response := "Hello! I'm Tony."
		if m.callCount < len(m.responses) {
			response = m.responses[m.callCount]
		}
		m.callCount++

		ch <- vega.StreamEvent{Type: vega.StreamEventMessageStart}
		ch <- vega.StreamEvent{Type: vega.StreamEventContentStart}
		// Stream word by word
		words := strings.Split(response, " ")
		for i, word := range words {
			if i > 0 {
				ch <- vega.StreamEvent{Type: vega.StreamEventContentDelta, Delta: " "}
			}
			ch <- vega.StreamEvent{Type: vega.StreamEventContentDelta, Delta: word}
		}
		ch <- vega.StreamEvent{Type: vega.StreamEventContentEnd}
		ch <- vega.StreamEvent{Type: vega.StreamEventMessageEnd}
	}()
	return ch, nil
}

func createTestConfig() *dsl.Document {
	temp := 0.7
	return &dsl.Document{
		Name: "Test Config",
		Agents: map[string]*dsl.Agent{
			"Tony": {
				Name:        "Tony",
				Model:       "claude-sonnet-4-20250514",
				System:      "You are Tony, a CTO.",
				Temperature: &temp,
				Budget:      "$5.00",
				Tools:       []string{"spawn_agent", "create_project"},
			},
		},
	}
}

func setupTestServer(t *testing.T) (*Server, *mockLLM) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	t.Cleanup(func() {
		orch.Shutdown(context.Background())
	})

	config := createTestConfig()
	customTools := tools.NewPersonaTools(orch, config, "./work", ".", nil)
	srv := New(orch, config, customTools, 0, "./work")

	return srv, llm
}

func TestNewServer(t *testing.T) {
	srv, _ := setupTestServer(t)

	if srv == nil {
		t.Fatal("New() returned nil")
	}
	if srv.orch == nil {
		t.Error("Orchestrator not set")
	}
	if srv.config == nil {
		t.Error("Config not set")
	}
	if srv.sessions == nil {
		t.Error("Sessions map not initialized")
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("Status = %q, want %q", body["status"], "ok")
	}
}

func TestChatCompletionsEndpoint_MethodNotAllowed(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/chat/completions", nil)
	w := httptest.NewRecorder()

	srv.handleChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestChatCompletionsEndpoint_InvalidJSON(t *testing.T) {
	srv, _ := setupTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader("invalid json"))
	w := httptest.NewRecorder()

	srv.handleChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestChatCompletionsEndpoint_NoUserMessage(t *testing.T) {
	srv, _ := setupTestServer(t)

	body := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "You are helpful"},
		},
	}
	jsonBody, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestChatCompletionsEndpoint_NonStreaming(t *testing.T) {
	srv, llm := setupTestServer(t)
	llm.responses = []string{"Hello! I'm Tony, ready to help."}

	body := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello Tony"},
		},
		Stream: false,
	}
	jsonBody, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChatCompletions(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Status = %d, want %d. Body: %s", resp.StatusCode, http.StatusOK, string(bodyBytes))
	}

	var chatResp ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if chatResp.Object != "chat.completion" {
		t.Errorf("Object = %q, want %q", chatResp.Object, "chat.completion")
	}
	if len(chatResp.Choices) == 0 {
		t.Fatal("No choices in response")
	}
	if chatResp.Choices[0].Message.Role != "assistant" {
		t.Errorf("Role = %q, want %q", chatResp.Choices[0].Message.Role, "assistant")
	}
	if chatResp.Choices[0].Message.Content == "" {
		t.Error("Empty content in response")
	}
}

func TestChatCompletionsEndpoint_Streaming(t *testing.T) {
	srv, llm := setupTestServer(t)
	llm.responses = []string{"Hello from Tony"}

	body := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello"},
		},
		Stream: true,
	}
	jsonBody, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChatCompletions(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Status = %d, want %d. Body: %s", resp.StatusCode, http.StatusOK, string(bodyBytes))
	}

	// Check content type for SSE
	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", contentType, "text/event-stream")
	}

	// Read and verify SSE data
	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)

	if !strings.Contains(bodyStr, "data:") {
		t.Error("Response should contain SSE data events")
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Error("Response should end with [DONE]")
	}
}

func TestChatCompletionsEndpoint_WithCallerPhone(t *testing.T) {
	srv, llm := setupTestServer(t)
	llm.responses = []string{"Hello caller!"}

	body := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Who am I?"},
		},
		Stream: false,
	}
	jsonBody, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vapi-Caller-Phone", "+1-555-123-4567")
	w := httptest.NewRecorder()

	srv.handleChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("Status = %d, want %d. Body: %s", resp.StatusCode, http.StatusOK, string(bodyBytes))
	}
}

func TestSessionManagement(t *testing.T) {
	srv, llm := setupTestServer(t)
	llm.responses = []string{"Response 1", "Response 2"}

	// First request creates session
	body := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello"},
		},
	}
	jsonBody, _ := json.Marshal(body)

	req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(jsonBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Vapi-Caller-Phone", "+1-555-111-1111")
	w1 := httptest.NewRecorder()
	srv.handleChatCompletions(w1, req1)

	// Check session was created
	srv.sessionsMu.RLock()
	sessionCount := len(srv.sessions)
	srv.sessionsMu.RUnlock()

	if sessionCount != 1 {
		t.Errorf("Session count = %d, want 1", sessionCount)
	}

	// Second request with same caller should reuse session
	req2 := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(jsonBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Vapi-Caller-Phone", "+1-555-111-1111")
	w2 := httptest.NewRecorder()
	srv.handleChatCompletions(w2, req2)

	srv.sessionsMu.RLock()
	sessionCount = len(srv.sessions)
	srv.sessionsMu.RUnlock()

	if sessionCount != 1 {
		t.Errorf("Session count after reuse = %d, want 1", sessionCount)
	}
}

func TestCleanupStaleSessions(t *testing.T) {
	srv, _ := setupTestServer(t)

	// This test just verifies the cleanup doesn't panic
	// In a real scenario, we'd need to mock completed/failed processes
	srv.CleanupStaleSessions(time.Hour)
}

func TestParseBudget(t *testing.T) {
	tests := []struct {
		input   string
		wantNil bool
		want    float64
	}{
		{"$5.00", false, 5.00},
		{"$10.50", false, 10.50},
		{"5.00", false, 5.00},
		{"invalid", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseBudget(tt.input)
			if tt.wantNil && got != nil {
				t.Errorf("parseBudget(%q) = %v, want nil", tt.input, got)
			}
			if !tt.wantNil && got == nil {
				t.Errorf("parseBudget(%q) = nil, want %v", tt.input, tt.want)
			}
			if !tt.wantNil && got != nil && got.Limit != tt.want {
				t.Errorf("parseBudget(%q).Limit = %v, want %v", tt.input, got.Limit, tt.want)
			}
		})
	}
}

func TestChatCompletionRequestParsing(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "valid request",
			json: `{"messages":[{"role":"user","content":"Hello"}],"stream":false}`,
		},
		{
			name: "with model",
			json: `{"model":"tony","messages":[{"role":"user","content":"Hi"}]}`,
		},
		{
			name: "with temperature",
			json: `{"messages":[{"role":"user","content":"Hi"}],"temperature":0.7}`,
		},
		{
			name:    "invalid json",
			json:    `{invalid}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req ChatCompletionRequest
			err := json.Unmarshal([]byte(tt.json), &req)
			if (err != nil) != tt.wantErr {
				t.Errorf("Unmarshal error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestChatCompletionResponseFormat(t *testing.T) {
	resp := ChatCompletionResponse{
		ID:      "test-123",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "tony",
		Choices: []Choice{{
			Index: 0,
			Message: &ChatMessage{
				Role:    "assistant",
				Content: "Hello!",
			},
		}},
	}

	jsonBytes, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	// Verify it can be unmarshaled back
	var decoded ChatCompletionResponse
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if decoded.ID != resp.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, resp.ID)
	}
	if decoded.Object != resp.Object {
		t.Errorf("Object = %q, want %q", decoded.Object, resp.Object)
	}
}
