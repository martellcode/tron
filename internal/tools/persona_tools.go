package tools

import (
	"context"
	"fmt"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vegaops/vega"
	"github.com/vegaops/vega/dsl"
	"gopkg.in/yaml.v3"
)

// PersonaTools provides Tony's orchestration tools
type PersonaTools struct {
	orch       *vega.Orchestrator
	config     *dsl.Document
	contacts   *ContactDB
	workingDir string
	tronDir    string

	// Track spawned agents and their callbacks
	callbacks   map[string]CallbackConfig
	callbacksMu sync.RWMutex

	// Memory storage
	directives    map[string]string
	personMemory  map[string]map[string]string
	directivesMu  sync.RWMutex
	personMemMu   sync.RWMutex
}

// CallbackConfig stores callback information for spawned agents
type CallbackConfig struct {
	Email   string
	Subject string
	SpawnedAt time.Time
}

// ContactDB provides contact lookup
type ContactDB struct {
	contacts map[string]Contact
	mu       sync.RWMutex
}

// Contact represents a contact entry
type Contact struct {
	Name    string            `yaml:"name"`
	Phone   string            `yaml:"phone"`
	Email   string            `yaml:"email"`
	Company string            `yaml:"company,omitempty"`
	Role    string            `yaml:"role,omitempty"`
	Notes   string            `yaml:"notes,omitempty"`
	Tags    []string          `yaml:"tags,omitempty"`
	Meta    map[string]string `yaml:"meta,omitempty"`
}

// NewPersonaTools creates a new PersonaTools instance
func NewPersonaTools(orch *vega.Orchestrator, config *dsl.Document, workingDir, tronDir string) *PersonaTools {
	pt := &PersonaTools{
		orch:         orch,
		config:       config,
		contacts:     &ContactDB{contacts: make(map[string]Contact)},
		workingDir:   workingDir,
		tronDir:      tronDir,
		callbacks:    make(map[string]CallbackConfig),
		directives:   make(map[string]string),
		personMemory: make(map[string]map[string]string),
	}

	// Load contacts from knowledge directory (check tron dir first, then current dir)
	knowledgeDir := filepath.Join(tronDir, "knowledge")
	if err := pt.loadContacts(filepath.Join(knowledgeDir, "contacts.yaml")); err != nil {
		// Try current directory as fallback
		pt.loadContacts("knowledge/contacts.yaml")
	}

	return pt
}

// loadContacts loads contacts from a YAML file
func (pt *PersonaTools) loadContacts(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err // File may not exist yet
	}

	var contactList struct {
		Contacts []Contact `yaml:"contacts"`
	}
	if err := yaml.Unmarshal(data, &contactList); err != nil {
		return err
	}

	pt.contacts.mu.Lock()
	defer pt.contacts.mu.Unlock()

	for _, c := range contactList.Contacts {
		// Index by phone number (normalized)
		phone := normalizePhone(c.Phone)
		pt.contacts.contacts[phone] = c
	}

	return nil
}

// normalizePhone strips non-numeric characters from phone numbers
func normalizePhone(phone string) string {
	var normalized strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

// RegisterTo registers all persona tools to a vega.Tools instance
func (pt *PersonaTools) RegisterTo(tools *vega.Tools) {
	// spawn_agent - Delegate work to a team member
	tools.Register("spawn_agent", vega.ToolDef{
		Description: "Spawn a team member agent to handle a task. Returns the process ID.",
		Fn:          pt.spawnAgent,
		Params: map[string]vega.ParamDef{
			"agent": {
				Type:        "string",
				Description: "Name of the team member (Gary, Sarah, Derek, Kathy, Tiffany, Marcus, Vera, Claire, Leo)",
				Required:    true,
			},
			"task": {
				Type:        "string",
				Description: "Description of the task to delegate",
				Required:    true,
			},
			"context": {
				Type:        "string",
				Description: "Additional context or files to provide",
				Required:    false,
			},
		},
	})

	// schedule_callback - Request notification when work completes
	tools.Register("schedule_callback", vega.ToolDef{
		Description: "Schedule an email notification when a spawned agent completes its work",
		Fn:          pt.scheduleCallback,
		Params: map[string]vega.ParamDef{
			"process_id": {
				Type:        "string",
				Description: "Process ID from spawn_agent",
				Required:    true,
			},
			"email": {
				Type:        "string",
				Description: "Email address to notify",
				Required:    true,
			},
			"subject": {
				Type:        "string",
				Description: "Subject line for the notification email",
				Required:    false,
			},
		},
	})

	// identify_caller - Look up caller by phone number
	tools.Register("identify_caller", vega.ToolDef{
		Description: "Look up a caller by their phone number",
		Fn:          pt.identifyCallerTool,
		Params: map[string]vega.ParamDef{
			"phone": {
				Type:        "string",
				Description: "Phone number to look up",
				Required:    true,
			},
		},
	})

	// create_project - Set up a new project workspace
	tools.Register("create_project", vega.ToolDef{
		Description: "Create a new project workspace in the work directory",
		Fn:          pt.createProject,
		Params: map[string]vega.ParamDef{
			"name": {
				Type:        "string",
				Description: "Project name (will be used as directory name)",
				Required:    true,
			},
			"description": {
				Type:        "string",
				Description: "Brief description of the project",
				Required:    false,
			},
			"template": {
				Type:        "string",
				Description: "Project template (go, python, node, react, empty)",
				Required:    false,
			},
		},
	})

	// save_directive - Save an important instruction
	tools.Register("save_directive", vega.ToolDef{
		Description: "Save an important instruction or directive for future reference",
		Fn:          pt.saveDirective,
		Params: map[string]vega.ParamDef{
			"key": {
				Type:        "string",
				Description: "Short key to identify the directive",
				Required:    true,
			},
			"directive": {
				Type:        "string",
				Description: "The directive or instruction to remember",
				Required:    true,
			},
		},
	})

	// save_person_memory - Remember facts about a person
	tools.Register("save_person_memory", vega.ToolDef{
		Description: "Save facts about a person for future conversations",
		Fn:          pt.savePersonMemory,
		Params: map[string]vega.ParamDef{
			"person": {
				Type:        "string",
				Description: "Person's name or identifier",
				Required:    true,
			},
			"key": {
				Type:        "string",
				Description: "Category of fact (e.g., 'preference', 'project', 'context')",
				Required:    true,
			},
			"fact": {
				Type:        "string",
				Description: "The fact to remember",
				Required:    true,
			},
		},
	})

	// web_search - Search the web
	tools.Register("web_search", vega.ToolDef{
		Description: "Search the web for current information (stub - implement with real search API)",
		Fn:          pt.webSearch,
		Params: map[string]vega.ParamDef{
			"query": {
				Type:        "string",
				Description: "Search query",
				Required:    true,
			},
		},
	})
}

// spawnAgent spawns a team member agent
func (pt *PersonaTools) spawnAgent(ctx context.Context, params map[string]any) (string, error) {
	agentName, _ := params["agent"].(string)
	task, _ := params["task"].(string)
	context, _ := params["context"].(string)

	// Get agent definition from config
	agentDef, ok := pt.config.Agents[agentName]
	if !ok {
		return "", fmt.Errorf("unknown team member: %s", agentName)
	}

	// Build the agent
	vegaTools := vega.NewTools(vega.WithSandbox(pt.workingDir))
	vegaTools.RegisterBuiltins()

	if len(agentDef.Tools) > 0 {
		vegaTools = vegaTools.Filter(agentDef.Tools...)
	}

	agent := vega.Agent{
		Name:   agentDef.Name,
		Model:  agentDef.Model,
		System: vega.StaticPrompt(agentDef.System),
		Tools:  vegaTools,
	}

	if agentDef.Temperature != nil {
		agent.Temperature = agentDef.Temperature
	}
	if agentDef.Budget != "" {
		agent.Budget = parseBudget(agentDef.Budget)
	}

	// Create supervision from config
	var supervision vega.Supervision
	if agentDef.Supervision != nil {
		supervision = vega.Supervision{
			Strategy:    parseStrategy(agentDef.Supervision.Strategy),
			MaxRestarts: agentDef.Supervision.MaxRestarts,
			Window:      parseWindow(agentDef.Supervision.Window),
		}
	} else {
		supervision = vega.Supervision{
			Strategy:    vega.Restart,
			MaxRestarts: 3,
		}
	}

	// Spawn the process
	proc, err := pt.orch.Spawn(agent,
		vega.WithSupervision(supervision),
		vega.WithTask(task),
		vega.WithWorkDir(pt.workingDir),
	)
	if err != nil {
		return "", fmt.Errorf("failed to spawn %s: %w", agentName, err)
	}

	// Send the initial task
	fullTask := task
	if context != "" {
		fullTask = fmt.Sprintf("%s\n\nContext:\n%s", task, context)
	}

	// Fire and forget - let the agent work
	proc.SendAsync(fullTask)

	return fmt.Sprintf("Spawned %s (process ID: %s) to work on: %s", agentName, proc.ID, task), nil
}

// parseStrategy converts a string strategy to vega.Strategy
func parseStrategy(s string) vega.Strategy {
	switch strings.ToLower(s) {
	case "restart":
		return vega.Restart
	case "stop":
		return vega.Stop
	case "escalate":
		return vega.Escalate
	case "restartall":
		return vega.RestartAll
	default:
		return vega.Restart
	}
}

// parseWindow converts a duration string like "10m" to time.Duration
func parseWindow(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
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

// scheduleCallback schedules a notification callback
func (pt *PersonaTools) scheduleCallback(ctx context.Context, params map[string]any) (string, error) {
	processID, _ := params["process_id"].(string)
	email, _ := params["email"].(string)
	subject, _ := params["subject"].(string)

	if subject == "" {
		subject = "Task Completed"
	}

	// Get the process
	proc := pt.orch.Get(processID)
	if proc == nil {
		return "", fmt.Errorf("process not found: %s", processID)
	}

	pt.callbacksMu.Lock()
	pt.callbacks[processID] = CallbackConfig{
		Email:     email,
		Subject:   subject,
		SpawnedAt: time.Now(),
	}
	pt.callbacksMu.Unlock()

	// Note: OnProcessComplete is a global callback, so we check the process ID in the callback
	// This is a one-time setup - multiple schedules for different processes are okay
	pt.setupCallbackHandlerOnce()

	return fmt.Sprintf("Callback scheduled. Will notify %s when process %s completes.", email, processID), nil
}

var callbackHandlerSetup sync.Once

// setupCallbackHandlerOnce sets up the global completion callback (only once)
func (pt *PersonaTools) setupCallbackHandlerOnce() {
	callbackHandlerSetup.Do(func() {
		pt.orch.OnProcessComplete(func(p *vega.Process, result string) {
			pt.callbacksMu.RLock()
			callback, ok := pt.callbacks[p.ID]
			pt.callbacksMu.RUnlock()

			if ok {
				pt.sendCallbackEmail(callback.Email, callback.Subject, result)

				// Clean up after sending
				pt.callbacksMu.Lock()
				delete(pt.callbacks, p.ID)
				pt.callbacksMu.Unlock()
			}
		})
	})
}

// sendCallbackEmail sends a notification email
func (pt *PersonaTools) sendCallbackEmail(to, subject, body string) error {
	smtpHost := os.Getenv("SMTP_HOST")
	smtpPort := os.Getenv("SMTP_PORT")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	fromEmail := os.Getenv("SMTP_FROM")

	if smtpHost == "" {
		// Log but don't fail if SMTP not configured
		fmt.Printf("SMTP not configured, would send email to %s: %s\n", to, subject)
		return nil
	}

	if smtpPort == "" {
		smtpPort = "587"
	}
	if fromEmail == "" {
		fromEmail = smtpUser
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		fromEmail, to, subject, body)

	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	return smtp.SendMail(smtpHost+":"+smtpPort, auth, fromEmail, []string{to}, []byte(msg))
}

// identifyCallerTool wraps IdentifyCaller as a tool
func (pt *PersonaTools) identifyCallerTool(ctx context.Context, params map[string]any) (string, error) {
	phone, _ := params["phone"].(string)
	result := pt.IdentifyCaller(phone)
	if result == "" {
		return "Unknown caller", nil
	}
	return result, nil
}

// IdentifyCaller looks up a caller by phone number (exported for server use)
func (pt *PersonaTools) IdentifyCaller(phone string) string {
	normalized := normalizePhone(phone)

	pt.contacts.mu.RLock()
	defer pt.contacts.mu.RUnlock()

	contact, ok := pt.contacts.contacts[normalized]
	if !ok {
		return ""
	}

	var info strings.Builder
	info.WriteString(fmt.Sprintf("Name: %s\n", contact.Name))
	if contact.Company != "" {
		info.WriteString(fmt.Sprintf("Company: %s\n", contact.Company))
	}
	if contact.Role != "" {
		info.WriteString(fmt.Sprintf("Role: %s\n", contact.Role))
	}
	if contact.Email != "" {
		info.WriteString(fmt.Sprintf("Email: %s\n", contact.Email))
	}
	if contact.Notes != "" {
		info.WriteString(fmt.Sprintf("Notes: %s\n", contact.Notes))
	}
	if len(contact.Tags) > 0 {
		info.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(contact.Tags, ", ")))
	}

	return info.String()
}

// createProject creates a new project workspace
func (pt *PersonaTools) createProject(ctx context.Context, params map[string]any) (string, error) {
	name, _ := params["name"].(string)
	description, _ := params["description"].(string)
	template, _ := params["template"].(string)

	// Sanitize project name
	safeName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)

	projectDir := filepath.Join(pt.workingDir, "projects", safeName)

	// Create directory
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create project directory: %w", err)
	}

	// Create README
	readme := fmt.Sprintf("# %s\n\n%s\n\nCreated: %s\n", name, description, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(projectDir, "README.md"), []byte(readme), 0644); err != nil {
		return "", fmt.Errorf("failed to create README: %w", err)
	}

	// Apply template if specified
	if template != "" {
		if err := pt.applyTemplate(projectDir, template); err != nil {
			return "", fmt.Errorf("failed to apply template: %w", err)
		}
	}

	return fmt.Sprintf("Created project '%s' at %s", name, projectDir), nil
}

// applyTemplate applies a project template
func (pt *PersonaTools) applyTemplate(dir, template string) error {
	switch template {
	case "go":
		return os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`), 0644)

	case "python":
		return os.WriteFile(filepath.Join(dir, "main.py"), []byte(`#!/usr/bin/env python3

def main():
    print("Hello, World!")

if __name__ == "__main__":
    main()
`), 0644)

	case "node":
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
  "name": "project",
  "version": "1.0.0",
  "main": "index.js"
}
`), 0644)
		return os.WriteFile(filepath.Join(dir, "index.js"), []byte(`console.log("Hello, World!");
`), 0644)

	case "react":
		// Just create a basic structure
		os.MkdirAll(filepath.Join(dir, "src"), 0755)
		return os.WriteFile(filepath.Join(dir, "src", "App.jsx"), []byte(`export default function App() {
  return <h1>Hello, World!</h1>;
}
`), 0644)

	case "empty":
		return nil

	default:
		return fmt.Errorf("unknown template: %s", template)
	}
}

// saveDirective saves a directive
func (pt *PersonaTools) saveDirective(ctx context.Context, params map[string]any) (string, error) {
	key, _ := params["key"].(string)
	directive, _ := params["directive"].(string)

	pt.directivesMu.Lock()
	pt.directives[key] = directive
	pt.directivesMu.Unlock()

	// Persist to file
	pt.persistDirectives()

	return fmt.Sprintf("Saved directive '%s': %s", key, directive), nil
}

// persistDirectives saves directives to disk
func (pt *PersonaTools) persistDirectives() error {
	pt.directivesMu.RLock()
	defer pt.directivesMu.RUnlock()

	data, err := yaml.Marshal(pt.directives)
	if err != nil {
		return err
	}

	knowledgeDir := filepath.Join(pt.tronDir, "knowledge")
	os.MkdirAll(knowledgeDir, 0755)
	return os.WriteFile(filepath.Join(knowledgeDir, "directives.yaml"), data, 0644)
}

// savePersonMemory saves memory about a person
func (pt *PersonaTools) savePersonMemory(ctx context.Context, params map[string]any) (string, error) {
	person, _ := params["person"].(string)
	key, _ := params["key"].(string)
	fact, _ := params["fact"].(string)

	pt.personMemMu.Lock()
	if pt.personMemory[person] == nil {
		pt.personMemory[person] = make(map[string]string)
	}
	pt.personMemory[person][key] = fact
	pt.personMemMu.Unlock()

	// Persist to file
	pt.persistPersonMemory()

	return fmt.Sprintf("Remembered about %s: %s = %s", person, key, fact), nil
}

// persistPersonMemory saves person memory to disk
func (pt *PersonaTools) persistPersonMemory() error {
	pt.personMemMu.RLock()
	defer pt.personMemMu.RUnlock()

	data, err := yaml.Marshal(pt.personMemory)
	if err != nil {
		return err
	}

	knowledgeDir := filepath.Join(pt.tronDir, "knowledge")
	os.MkdirAll(knowledgeDir, 0755)
	return os.WriteFile(filepath.Join(knowledgeDir, "person_memory.yaml"), data, 0644)
}

// webSearch performs a web search (stub implementation)
func (pt *PersonaTools) webSearch(ctx context.Context, params map[string]any) (string, error) {
	query, _ := params["query"].(string)

	// This is a stub - in production, integrate with a real search API
	// like Brave Search, SerpAPI, or similar
	searchAPI := os.Getenv("SEARCH_API_KEY")
	if searchAPI == "" {
		return fmt.Sprintf("Web search for '%s' is not configured. Set SEARCH_API_KEY to enable.", query), nil
	}

	// TODO: Implement actual search API call
	return fmt.Sprintf("Searched for: %s (implement actual search API)", query), nil
}
