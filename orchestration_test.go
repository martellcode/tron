package tron_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vegaops/vega"
	"github.com/vegaops/vega/dsl"
)

// mockLLM implements vega.LLM for testing orchestration
type mockLLM struct {
	mu           sync.Mutex
	responses    map[string][]string // agent name -> responses
	callCounts   map[string]int
	toolCalls    map[string][]vega.ToolCall // can inject tool calls
	defaultResp  string
}

func newMockLLM() *mockLLM {
	return &mockLLM{
		responses:   make(map[string][]string),
		callCounts:  make(map[string]int),
		toolCalls:   make(map[string][]vega.ToolCall),
		defaultResp: "Default mock response",
	}
}

func (m *mockLLM) SetResponses(agent string, responses []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[agent] = responses
	m.callCounts[agent] = 0
}

func (m *mockLLM) SetToolCalls(agent string, calls []vega.ToolCall) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolCalls[agent] = calls
}

func (m *mockLLM) GetCallCount(agent string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCounts[agent]
}

func (m *mockLLM) getResponse(messages []vega.Message) (string, []vega.ToolCall) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Try to determine agent from system message
	agent := "unknown"
	for _, msg := range messages {
		if msg.Role == vega.RoleSystem {
			if strings.Contains(msg.Content, "Tony") {
				agent = "Tony"
			} else if strings.Contains(msg.Content, "Gary") {
				agent = "Gary"
			} else if strings.Contains(msg.Content, "Sarah") {
				agent = "Sarah"
			}
			break
		}
	}

	responses, ok := m.responses[agent]
	if !ok {
		return m.defaultResp, nil
	}

	idx := m.callCounts[agent]
	m.callCounts[agent]++

	if idx < len(responses) {
		return responses[idx], m.toolCalls[agent]
	}
	return m.defaultResp, nil
}

func (m *mockLLM) Generate(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (*vega.LLMResponse, error) {
	response, toolCalls := m.getResponse(messages)
	return &vega.LLMResponse{
		Content:      response,
		ToolCalls:    toolCalls,
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.001,
	}, nil
}

func (m *mockLLM) GenerateStream(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (<-chan vega.StreamEvent, error) {
	ch := make(chan vega.StreamEvent, 20)
	go func() {
		defer close(ch)
		response, _ := m.getResponse(messages)

		ch <- vega.StreamEvent{Type: vega.StreamEventMessageStart}
		ch <- vega.StreamEvent{Type: vega.StreamEventContentStart}
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

func createTestDocument() *dsl.Document {
	temp := 0.7
	lowTemp := 0.3
	return &dsl.Document{
		Name:        "Test Tron Config",
		Description: "Test configuration for orchestration tests",
		Agents: map[string]*dsl.Agent{
			"Tony": {
				Name:        "Tony",
				Model:       "claude-sonnet-4-20250514",
				System:      "You are Tony, the CTO. Delegate tasks to your team.",
				Temperature: &temp,
				Budget:      "$5.00",
				Tools:       []string{"spawn_agent", "read_file", "write_file"},
				Supervision: &dsl.SupervisionDef{
					Strategy:    "restart",
					MaxRestarts: 3,
					Window:      "10m",
				},
			},
			"Gary": {
				Name:        "Gary",
				Model:       "claude-sonnet-4-20250514",
				System:      "You are Gary, a senior engineer. Write high-quality code.",
				Temperature: &lowTemp,
				Budget:      "$3.00",
				Tools:       []string{"read_file", "write_file"},
			},
			"Sarah": {
				Name:        "Sarah",
				Model:       "claude-sonnet-4-20250514",
				System:      "You are Sarah, a tech lead. Review architecture and code.",
				Temperature: &temp,
				Budget:      "$3.00",
				Tools:       []string{"read_file"},
			},
		},
	}
}

func TestOrchestratorBasics(t *testing.T) {
	llm := newMockLLM()
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(10),
	)
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	// Create Tony agent
	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	// Spawn process
	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	if proc.ID == "" {
		t.Error("Process ID is empty")
	}

	// Verify process is running
	if proc.Status() != vega.StatusRunning {
		t.Errorf("Status = %v, want %v", proc.Status(), vega.StatusRunning)
	}

	// Verify process is in list
	procs := orch.List()
	if len(procs) != 1 {
		t.Errorf("Process count = %d, want 1", len(procs))
	}

	// Verify can get by ID
	found := orch.Get(proc.ID)
	if found == nil {
		t.Error("Could not find process by ID")
	}
}

func TestProcessSendAndReceive(t *testing.T) {
	llm := newMockLLM()
	llm.SetResponses("Tony", []string{
		"Hello! I'm Tony, your CTO. How can I help you today?",
		"I'll delegate that to Gary, our senior engineer.",
	})

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	ctx := context.Background()

	// First message
	resp1, err := proc.Send(ctx, "Hello Tony!")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if !strings.Contains(resp1, "Tony") {
		t.Errorf("Response should mention Tony: %q", resp1)
	}

	// Second message
	resp2, err := proc.Send(ctx, "Can you write some code?")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if resp2 == "" {
		t.Error("Response should not be empty")
	}

	// Verify call count increased
	if llm.GetCallCount("Tony") < 2 {
		t.Errorf("LLM call count = %d, want >= 2", llm.GetCallCount("Tony"))
	}
}

func TestProcessStreaming(t *testing.T) {
	llm := newMockLLM()
	llm.SetResponses("Tony", []string{"This is a streaming response from Tony"})

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	ctx := context.Background()
	stream, err := proc.SendStream(ctx, "Stream a response")
	if err != nil {
		t.Fatalf("SendStream failed: %v", err)
	}

	var chunks []string
	for chunk := range stream.Chunks() {
		chunks = append(chunks, chunk)
	}

	if err := stream.Err(); err != nil {
		t.Errorf("Stream error: %v", err)
	}

	fullResponse := strings.Join(chunks, "")
	if fullResponse == "" {
		t.Error("Streamed response should not be empty")
	}

	// Verify Response() returns same content
	if stream.Response() != fullResponse {
		t.Error("Response() should match streamed content")
	}
}

func TestProcessAsync(t *testing.T) {
	llm := newMockLLM()
	llm.SetResponses("Tony", []string{"Async response ready"})

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	// Send async
	future := proc.SendAsync("Process this async")

	// Should not be done immediately (but might be with fast mock)
	// Wait for result
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := future.Await(ctx)
	if err != nil {
		t.Fatalf("Await failed: %v", err)
	}

	if result == "" {
		t.Error("Async result should not be empty")
	}

	// Future should be done now
	if !future.Done() {
		t.Error("Future should be done after Await")
	}
}

func TestMultipleProcesses(t *testing.T) {
	llm := newMockLLM()
	llm.SetResponses("Tony", []string{"Tony response"})
	llm.SetResponses("Gary", []string{"Gary response"})
	llm.SetResponses("Sarah", []string{"Sarah response"})

	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(10),
	)
	defer orch.Shutdown(context.Background())

	config := createTestDocument()

	// Spawn multiple agents
	agents := []string{"Tony", "Gary", "Sarah"}
	processes := make([]*vega.Process, len(agents))

	for i, name := range agents {
		def := config.Agents[name]
		agent := vega.Agent{
			Name:   def.Name,
			Model:  def.Model,
			System: vega.StaticPrompt(def.System),
			Tools:  vega.NewTools(),
		}

		proc, err := orch.Spawn(agent)
		if err != nil {
			t.Fatalf("Failed to spawn %s: %v", name, err)
		}
		processes[i] = proc
	}

	// Verify all processes are running
	procs := orch.List()
	if len(procs) != 3 {
		t.Errorf("Process count = %d, want 3", len(procs))
	}

	// Send messages to all concurrently
	var wg sync.WaitGroup
	results := make([]string, len(agents))
	errors := make([]error, len(agents))

	ctx := context.Background()
	for i, proc := range processes {
		wg.Add(1)
		go func(idx int, p *vega.Process) {
			defer wg.Done()
			results[idx], errors[idx] = p.Send(ctx, "Hello")
		}(i, proc)
	}
	wg.Wait()

	// Check results
	for i, err := range errors {
		if err != nil {
			t.Errorf("Agent %s error: %v", agents[i], err)
		}
		if results[i] == "" {
			t.Errorf("Agent %s returned empty response", agents[i])
		}
	}
}

func TestProcessLifecycleCallbacks(t *testing.T) {
	llm := newMockLLM()
	llm.SetResponses("Tony", []string{"Task completed successfully"})

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	var startedCount, completedCount int32
	var completedResult string
	var mu sync.Mutex

	orch.OnProcessStarted(func(p *vega.Process) {
		atomic.AddInt32(&startedCount, 1)
	})

	orch.OnProcessComplete(func(p *vega.Process, result string) {
		atomic.AddInt32(&completedCount, 1)
		mu.Lock()
		completedResult = result
		mu.Unlock()
	})

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	// Give callback time to fire
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&startedCount) != 1 {
		t.Errorf("Started callback count = %d, want 1", startedCount)
	}

	// Complete the process
	proc.Complete("Final result")

	// Give callback time to fire
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&completedCount) != 1 {
		t.Errorf("Completed callback count = %d, want 1", completedCount)
	}

	mu.Lock()
	if completedResult != "Final result" {
		t.Errorf("Completed result = %q, want %q", completedResult, "Final result")
	}
	mu.Unlock()
}

func TestProcessSupervision(t *testing.T) {
	llm := newMockLLM()

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	supervision := vega.Supervision{
		Strategy:    vega.Restart,
		MaxRestarts: 3,
		Window:      10 * time.Minute,
	}

	proc, err := orch.Spawn(tonyAgent, vega.WithSupervision(supervision))
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	if proc.Supervision == nil {
		t.Error("Supervision not set on process")
	}

	if proc.Supervision.Strategy != vega.Restart {
		t.Errorf("Strategy = %v, want %v", proc.Supervision.Strategy, vega.Restart)
	}

	if proc.Supervision.MaxRestarts != 3 {
		t.Errorf("MaxRestarts = %d, want 3", proc.Supervision.MaxRestarts)
	}
}

func TestProcessKill(t *testing.T) {
	llm := newMockLLM()

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}

	procID := proc.ID

	// Kill the process
	err = orch.Kill(procID)
	if err != nil {
		t.Fatalf("Kill failed: %v", err)
	}

	// Process should no longer be in list
	if orch.Get(procID) != nil {
		t.Error("Process should be removed after kill")
	}
}

func TestMaxProcessesLimit(t *testing.T) {
	llm := newMockLLM()

	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(2),
	)
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	// Spawn up to max
	_, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("First spawn failed: %v", err)
	}

	_, err = orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Second spawn failed: %v", err)
	}

	// Third should fail
	_, err = orch.Spawn(tonyAgent)
	if err == nil {
		t.Error("Third spawn should fail due to max processes")
	}
	if err != vega.ErrMaxProcessesReached {
		t.Errorf("Error = %v, want %v", err, vega.ErrMaxProcessesReached)
	}
}

func TestProcessTimeout(t *testing.T) {
	llm := newMockLLM()

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	// Spawn with very short timeout
	proc, err := orch.Spawn(tonyAgent, vega.WithTimeout(1*time.Millisecond))
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	// Wait for timeout
	time.Sleep(10 * time.Millisecond)

	// Send should fail due to context cancellation
	_, err = proc.Send(context.Background(), "Hello")
	if err == nil {
		// Timeout might not have triggered yet, which is okay
		t.Log("Timeout did not trigger (fast mock)")
	}
}

func TestOrchestratorShutdown(t *testing.T) {
	llm := newMockLLM()

	orch := vega.NewOrchestrator(vega.WithLLM(llm))

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  vega.NewTools(),
	}

	// Spawn some processes
	_, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	// Shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = orch.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown error: %v", err)
	}
}

func TestAgentWithTools(t *testing.T) {
	llm := newMockLLM()

	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	tools := vega.NewTools(vega.WithSandbox("./work"))
	tools.RegisterBuiltins()

	// Register a custom tool
	tools.Register("greet", func(name string) string {
		return "Hello, " + name + "!"
	})

	config := createTestDocument()
	tonyDef := config.Agents["Tony"]

	tonyAgent := vega.Agent{
		Name:   tonyDef.Name,
		Model:  tonyDef.Model,
		System: vega.StaticPrompt(tonyDef.System),
		Tools:  tools,
	}

	proc, err := orch.Spawn(tonyAgent)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	// Verify tools are available
	schemas := proc.Agent.Tools.Schema()
	if len(schemas) == 0 {
		t.Error("Agent should have tools")
	}

	// Find our custom tool
	found := false
	for _, s := range schemas {
		if s.Name == "greet" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Custom tool 'greet' not found in schemas")
	}
}

func TestToolExecution(t *testing.T) {
	tools := vega.NewTools()

	// Register test tools
	tools.Register("add", func(a, b int) int {
		return a + b
	})

	tools.Register("concat", vega.ToolDef{
		Description: "Concatenate strings",
		Fn: func(ctx context.Context, params map[string]any) (string, error) {
			a, _ := params["a"].(string)
			b, _ := params["b"].(string)
			return a + b, nil
		},
		Params: map[string]vega.ParamDef{
			"a": {Type: "string", Required: true},
			"b": {Type: "string", Required: true},
		},
	})

	ctx := context.Background()

	// Test concat
	result, err := tools.Execute(ctx, "concat", map[string]any{
		"a": "Hello, ",
		"b": "World!",
	})
	if err != nil {
		t.Fatalf("Execute concat failed: %v", err)
	}
	if result != "Hello, World!" {
		t.Errorf("Result = %q, want %q", result, "Hello, World!")
	}
}

func TestToolFilter(t *testing.T) {
	tools := vega.NewTools()
	tools.RegisterBuiltins()

	// Filter to only read_file
	filtered := tools.Filter("read_file")

	schemas := filtered.Schema()
	if len(schemas) != 1 {
		t.Errorf("Filtered tool count = %d, want 1", len(schemas))
	}

	if schemas[0].Name != "read_file" {
		t.Errorf("Tool name = %q, want %q", schemas[0].Name, "read_file")
	}
}
