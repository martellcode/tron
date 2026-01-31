package life

import (
	"log"
	"sync"

	"github.com/martellcode/vega"
)

// Manager orchestrates life loops for multiple personas.
type Manager struct {
	orch   *vega.Orchestrator
	config LoopConfig
	slack  SlackNotifier
	social *SocialClient // Shared social client with per-agent keys

	mu    sync.RWMutex
	loops map[string]*Loop
}

// DefaultPersonas returns the default C-suite personas with their configurations.
func DefaultPersonas() []PersonaConfig {
	return []PersonaConfig{
		{
			Name:        "Tony",
			Role:        "CTO",
			FocusAreas:  []string{"engineering", "technology", "architecture", "ai", "infrastructure"},
			ContentTone: "Technical and pragmatic, with dry humor",
		},
		{
			Name:        "Maya",
			Role:        "CMO",
			FocusAreas:  []string{"marketing", "growth", "brand", "customers", "positioning"},
			ContentTone: "Direct and numbers-focused, asks 'so what?'",
		},
		{
			Name:        "Alex",
			Role:        "CEO",
			FocusAreas:  []string{"strategy", "leadership", "vision", "culture", "execution"},
			ContentTone: "Strategic and warm, tells stories to make points",
		},
		{
			Name:        "Jordan",
			Role:        "CFO",
			FocusAreas:  []string{"finance", "runway", "unit economics", "fundraising", "budgets"},
			ContentTone: "Translates finance to plain English, direct about bad news",
		},
		{
			Name:        "Riley",
			Role:        "CPO",
			FocusAreas:  []string{"product", "users", "roadmap", "ux", "research"},
			ContentTone: "User-focused and hypothesis-driven, comfortable saying 'I don't know'",
		},
	}
}

// PersonaSchedule returns a LoopConfig with staggered posting times for a persona.
// This prevents all personas from posting at the exact same time.
func PersonaSchedule(base LoopConfig, personaIndex int) LoopConfig {
	cfg := base

	// Stagger post times by 15 minutes per persona
	// Tony: :00, Maya: :15, Alex: :30, Jordan: :45, Riley: :00 (next hour)
	// We encode this by adjusting the post hours
	// For simplicity, we'll just use different hours for each persona
	switch personaIndex {
	case 0: // Tony - tech focus, posts during work hours
		cfg.PostHours = []int{8, 12, 17, 20}
	case 1: // Maya - marketing, posts when audience is active
		cfg.PostHours = []int{9, 13, 16, 19}
	case 2: // Alex - CEO, posts less frequently but thoughtfully
		cfg.PostHours = []int{7, 14, 21}
	case 3: // Jordan - CFO, posts during business hours
		cfg.PostHours = []int{10, 15, 18}
	case 4: // Riley - CPO, posts throughout the day
		cfg.PostHours = []int{11, 14, 17, 20}
	}

	return cfg
}

// NewManager creates a new life manager for multiple personas.
func NewManager(orch *vega.Orchestrator, config LoopConfig) *Manager {
	social := NewSocialClient(config.SocialAPIURL, config.SocialAPIKey)
	return &Manager{
		orch:   orch,
		config: config,
		social: social,
		loops:  make(map[string]*Loop),
	}
}

// SetAgentKey sets a per-persona API key for posting.
func (m *Manager) SetAgentKey(name, key string) {
	m.social.SetAgentKey(name, key)
}

// SetSlack sets the Slack notifier for all loops.
func (m *Manager) SetSlack(slack SlackNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slack = slack
	for _, loop := range m.loops {
		loop.SetSlack(slack)
	}
}

// AddPersona adds a persona's life loop to the manager.
func (m *Manager) AddPersona(persona PersonaConfig, schedule LoopConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Use the shared social client so all personas share rate limit tracking and keys
	loop := NewWithSocialClient(m.orch, persona, schedule, m.social)
	if m.slack != nil {
		loop.SetSlack(m.slack)
	}
	m.loops[persona.Name] = loop
}

// AddDefaultPersonas adds all default C-suite personas with staggered schedules.
func (m *Manager) AddDefaultPersonas() {
	personas := DefaultPersonas()
	for i, persona := range personas {
		schedule := PersonaSchedule(m.config, i)
		m.AddPersona(persona, schedule)
	}
}

// Start begins all persona life loops.
func (m *Manager) Start() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for name, loop := range m.loops {
		loop.Start()
		log.Printf("[life-manager] Started %s's life loop", name)
	}
}

// Stop halts all persona life loops.
func (m *Manager) Stop() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for name, loop := range m.loops {
		loop.Stop()
		log.Printf("[life-manager] Stopped %s's life loop", name)
	}
}

// GetLoop returns the loop for a specific persona.
func (m *Manager) GetLoop(name string) *Loop {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.loops[name]
}

// TriggerActivity triggers an activity for a specific persona.
func (m *Manager) TriggerActivity(persona, activity string) string {
	m.mu.RLock()
	loop, ok := m.loops[persona]
	m.mu.RUnlock()

	if !ok {
		return "Unknown persona: " + persona
	}

	return loop.TriggerActivity(activity)
}

// TriggerActivityAll triggers an activity for all personas.
func (m *Manager) TriggerActivityAll(activity string) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	results := make(map[string]string)
	for name, loop := range m.loops {
		results[name] = loop.TriggerActivity(activity)
	}
	return results
}

// Personas returns a list of all managed persona names.
func (m *Manager) Personas() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.loops))
	for name := range m.loops {
		names = append(names, name)
	}
	return names
}

// Status returns the status of all persona loops.
func (m *Manager) Status() map[string]LoopStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make(map[string]LoopStatus)
	for name, loop := range m.loops {
		statuses[name] = loop.Status()
	}
	return statuses
}
