package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vegaops/vega"
	"github.com/vegaops/vega/dsl"
)

// mockLLM implements vega.LLM for testing
type mockLLM struct {
	responses []string
	callCount int
}

func (m *mockLLM) Generate(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (*vega.LLMResponse, error) {
	response := "Mock response"
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
		response := "Mock streamed response"
		if m.callCount < len(m.responses) {
			response = m.responses[m.callCount]
		}
		m.callCount++

		ch <- vega.StreamEvent{Type: vega.StreamEventMessageStart}
		ch <- vega.StreamEvent{Type: vega.StreamEventContentStart}
		for _, word := range []string{response} {
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
			"Gary": {
				Name:        "Gary",
				Model:       "claude-sonnet-4-20250514",
				System:      "You are Gary, a senior engineer.",
				Temperature: &temp,
				Tools:       []string{"read_file", "write_file"},
				Supervision: &dsl.SupervisionDef{
					Strategy:    "restart",
					MaxRestarts: 3,
					Window:      "10m",
				},
			},
		},
	}
}

func TestNewPersonaTools(t *testing.T) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestConfig()
	pt := NewPersonaTools(orch, config, "./work", ".")

	if pt == nil {
		t.Fatal("NewPersonaTools returned nil")
	}
	if pt.orch != orch {
		t.Error("Orchestrator not set correctly")
	}
	if pt.config != config {
		t.Error("Config not set correctly")
	}
	if pt.contacts == nil {
		t.Error("ContactDB not initialized")
	}
	if pt.callbacks == nil {
		t.Error("Callbacks map not initialized")
	}
}

func TestRegisterTo(t *testing.T) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestConfig()
	pt := NewPersonaTools(orch, config, "./work", ".")

	tools := vega.NewTools()
	pt.RegisterTo(tools)

	// Verify tools were registered by checking schema
	schemas := tools.Schema()
	toolNames := make(map[string]bool)
	for _, s := range schemas {
		toolNames[s.Name] = true
	}

	expectedTools := []string{
		"spawn_agent",
		"schedule_callback",
		"identify_caller",
		"create_project",
		"save_directive",
		"save_person_memory",
		"web_search",
	}

	for _, name := range expectedTools {
		if !toolNames[name] {
			t.Errorf("Expected tool %q to be registered", name)
		}
	}
}

func TestCreateProject(t *testing.T) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestConfig()
	pt := NewPersonaTools(orch, config, "./work", ".")

	// Create temp directory for test
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Create work/projects directory
	os.MkdirAll("work/projects", 0755)

	ctx := context.Background()

	tests := []struct {
		name     string
		params   map[string]any
		wantErr  bool
		template string
	}{
		{
			name: "basic project",
			params: map[string]any{
				"name":        "test-project",
				"description": "A test project",
			},
			wantErr: false,
		},
		{
			name: "go template",
			params: map[string]any{
				"name":        "go-project",
				"description": "A Go project",
				"template":    "go",
			},
			wantErr:  false,
			template: "go",
		},
		{
			name: "python template",
			params: map[string]any{
				"name":        "py-project",
				"description": "A Python project",
				"template":    "python",
			},
			wantErr:  false,
			template: "python",
		},
		{
			name: "node template",
			params: map[string]any{
				"name":        "node-project",
				"description": "A Node project",
				"template":    "node",
			},
			wantErr:  false,
			template: "node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := pt.createProject(ctx, tt.params)
			if (err != nil) != tt.wantErr {
				t.Errorf("createProject() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if result == "" {
				t.Error("createProject() returned empty result")
			}

			// Verify project directory exists
			projectName := tt.params["name"].(string)
			projectDir := filepath.Join("work", "projects", projectName)
			if _, err := os.Stat(projectDir); os.IsNotExist(err) {
				t.Errorf("Project directory not created: %s", projectDir)
			}

			// Verify README exists
			readmePath := filepath.Join(projectDir, "README.md")
			if _, err := os.Stat(readmePath); os.IsNotExist(err) {
				t.Error("README.md not created")
			}

			// Verify template files
			switch tt.template {
			case "go":
				if _, err := os.Stat(filepath.Join(projectDir, "main.go")); os.IsNotExist(err) {
					t.Error("main.go not created for Go template")
				}
			case "python":
				if _, err := os.Stat(filepath.Join(projectDir, "main.py")); os.IsNotExist(err) {
					t.Error("main.py not created for Python template")
				}
			case "node":
				if _, err := os.Stat(filepath.Join(projectDir, "package.json")); os.IsNotExist(err) {
					t.Error("package.json not created for Node template")
				}
				if _, err := os.Stat(filepath.Join(projectDir, "index.js")); os.IsNotExist(err) {
					t.Error("index.js not created for Node template")
				}
			}
		})
	}
}

func TestIdentifyCaller(t *testing.T) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestConfig()
	pt := NewPersonaTools(orch, config, "./work", ".")

	// Add a test contact
	pt.contacts.mu.Lock()
	pt.contacts.contacts["15551234567"] = Contact{
		Name:    "John Doe",
		Phone:   "+1-555-123-4567",
		Email:   "john@example.com",
		Company: "Test Corp",
		Role:    "CEO",
		Notes:   "VIP customer",
		Tags:    []string{"vip", "client"},
	}
	pt.contacts.mu.Unlock()

	tests := []struct {
		name      string
		phone     string
		wantEmpty bool
		wantName  string
	}{
		{
			name:      "known contact",
			phone:     "+1-555-123-4567",
			wantEmpty: false,
			wantName:  "John Doe",
		},
		{
			name:      "known contact normalized",
			phone:     "15551234567",
			wantEmpty: false,
			wantName:  "John Doe",
		},
		{
			name:      "unknown contact",
			phone:     "+1-555-999-9999",
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pt.IdentifyCaller(tt.phone)
			if tt.wantEmpty && result != "" {
				t.Errorf("IdentifyCaller() = %q, want empty", result)
			}
			if !tt.wantEmpty && result == "" {
				t.Error("IdentifyCaller() returned empty, want contact info")
			}
			if !tt.wantEmpty && tt.wantName != "" {
				if !contains(result, tt.wantName) {
					t.Errorf("IdentifyCaller() = %q, should contain %q", result, tt.wantName)
				}
			}
		})
	}
}

func TestSaveDirective(t *testing.T) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestConfig()
	pt := NewPersonaTools(orch, config, "./work", ".")

	// Create temp directory for test
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	os.MkdirAll("knowledge", 0755)

	ctx := context.Background()
	params := map[string]any{
		"key":       "code-style",
		"directive": "Always use 4 spaces for indentation",
	}

	result, err := pt.saveDirective(ctx, params)
	if err != nil {
		t.Fatalf("saveDirective() error = %v", err)
	}

	if result == "" {
		t.Error("saveDirective() returned empty result")
	}

	// Verify directive was saved in memory
	pt.directivesMu.RLock()
	saved, ok := pt.directives["code-style"]
	pt.directivesMu.RUnlock()

	if !ok {
		t.Error("Directive not saved in memory")
	}
	if saved != "Always use 4 spaces for indentation" {
		t.Errorf("Directive value = %q, want %q", saved, "Always use 4 spaces for indentation")
	}
}

func TestSavePersonMemory(t *testing.T) {
	llm := &mockLLM{}
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	config := createTestConfig()
	pt := NewPersonaTools(orch, config, "./work", ".")

	// Create temp directory for test
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	os.MkdirAll("knowledge", 0755)

	ctx := context.Background()
	params := map[string]any{
		"person": "Alice",
		"key":    "preference",
		"fact":   "Prefers async communication",
	}

	result, err := pt.savePersonMemory(ctx, params)
	if err != nil {
		t.Fatalf("savePersonMemory() error = %v", err)
	}

	if result == "" {
		t.Error("savePersonMemory() returned empty result")
	}

	// Verify memory was saved
	pt.personMemMu.RLock()
	personMem, ok := pt.personMemory["Alice"]
	pt.personMemMu.RUnlock()

	if !ok {
		t.Error("Person memory not saved")
	}
	if personMem["preference"] != "Prefers async communication" {
		t.Errorf("Memory value = %q, want %q", personMem["preference"], "Prefers async communication")
	}
}

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"+1-555-123-4567", "15551234567"},
		{"(555) 123-4567", "5551234567"},
		{"555.123.4567", "5551234567"},
		{"15551234567", "15551234567"},
		{"+44 20 7946 0958", "442079460958"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizePhone(tt.input)
			if got != tt.want {
				t.Errorf("normalizePhone(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseStrategy(t *testing.T) {
	tests := []struct {
		input string
		want  vega.Strategy
	}{
		{"restart", vega.Restart},
		{"RESTART", vega.Restart},
		{"stop", vega.Stop},
		{"escalate", vega.Escalate},
		{"restartall", vega.RestartAll},
		{"unknown", vega.Restart}, // defaults to Restart
		{"", vega.Restart},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseStrategy(tt.input)
			if got != tt.want {
				t.Errorf("parseStrategy(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseWindow(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"10m", 10 * time.Minute},
		{"1h", time.Hour},
		{"30s", 30 * time.Second},
		{"", 0},
		{"invalid", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseWindow(tt.input)
			if got != tt.want {
				t.Errorf("parseWindow(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseBudget(t *testing.T) {
	tests := []struct {
		input   string
		wantNil bool
		want    float64
	}{
		{"$5.00", false, 5.00},
		{"$10.50", false, 10.50},
		{"$0.25", false, 0.25},
		{"5.00", false, 5.00}, // without $
		{"invalid", true, 0},
		{"", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseBudget(tt.input)
			if tt.wantNil && got != nil {
				t.Errorf("parseBudget(%q) = %v, want nil", tt.input, got)
			}
			if !tt.wantNil {
				if got == nil {
					t.Errorf("parseBudget(%q) = nil, want %v", tt.input, tt.want)
				} else if got.Limit != tt.want {
					t.Errorf("parseBudget(%q).Limit = %v, want %v", tt.input, got.Limit, tt.want)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
