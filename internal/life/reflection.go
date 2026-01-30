package life

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/martellcode/vega"
)

// Reflector handles Tony's self-improvement and learning.
type Reflector struct {
	orch    *vega.Orchestrator
	baseDir string

	// Learnings Tony has accumulated
	learnings []Learning

	// Self-improvement notes
	improvements []Improvement
}

// Learning represents something Tony learned.
type Learning struct {
	Time     time.Time `json:"time"`
	Category string    `json:"category"` // tech, people, process, self
	Content  string    `json:"content"`
	Source   string    `json:"source"` // reading, conversation, observation
}

// Improvement represents a self-improvement note.
type Improvement struct {
	Time        time.Time `json:"time"`
	Area        string    `json:"area"`        // communication, technical, leadership
	Observation string    `json:"observation"` // what Tony noticed
	Action      string    `json:"action"`      // what to do differently
	Applied     bool      `json:"applied"`
}

// NewReflector creates a new reflector.
func NewReflector(orch *vega.Orchestrator, baseDir string) *Reflector {
	r := &Reflector{
		orch:         orch,
		baseDir:      baseDir,
		learnings:    make([]Learning, 0),
		improvements: make([]Improvement, 0),
	}
	r.load()
	return r
}

// Reflect analyzes recent activity and generates insights.
func (r *Reflector) Reflect(ctx context.Context, journal []JournalEntry) (string, error) {
	if len(journal) == 0 {
		return "", nil
	}

	// Build context from journal
	var context strings.Builder
	context.WriteString("Recent activity:\n")
	for _, e := range journal {
		context.WriteString(fmt.Sprintf("- [%s] %s: %s\n",
			e.Time.Format("15:04"), e.Type, e.Content))
	}

	// TODO: Use vega process to generate deep reflection
	// For now, do pattern-based reflection

	insights := r.patternReflect(journal)
	if insights != "" {
		// Save as a learning
		r.AddLearning(Learning{
			Time:     time.Now(),
			Category: "self",
			Content:  insights,
			Source:   "reflection",
		})
	}

	return insights, nil
}

// patternReflect does rule-based reflection without LLM.
func (r *Reflector) patternReflect(journal []JournalEntry) string {
	var insights []string

	// Count activity types
	typeCounts := make(map[string]int)
	for _, e := range journal {
		typeCounts[e.Type]++
	}

	// Analyze patterns
	if typeCounts["reading"] > 5 {
		insights = append(insights, "Heavy reading day - make sure to apply what I learned")
	}
	if typeCounts["team"] == 0 {
		insights = append(insights, "Haven't checked on the team today - should do that")
	}
	if typeCounts["tweet"] > 3 {
		insights = append(insights, "Tweeted a lot today - quality over quantity")
	}

	// Check for work-life balance indicators
	var lateNightActivity int
	for _, e := range journal {
		if e.Time.Hour() >= 22 || e.Time.Hour() < 6 {
			lateNightActivity++
		}
	}
	if lateNightActivity > 2 {
		insights = append(insights, "Working late - remember to rest")
	}

	if len(insights) == 0 {
		return "Balanced day with good mix of activities."
	}

	return strings.Join(insights, ". ")
}

// AddLearning records a new learning.
func (r *Reflector) AddLearning(l Learning) {
	if l.Time.IsZero() {
		l.Time = time.Now()
	}
	r.learnings = append(r.learnings, l)

	// Keep last 100 learnings
	if len(r.learnings) > 100 {
		r.learnings = r.learnings[len(r.learnings)-100:]
	}

	r.persist()
}

// AddImprovement records a self-improvement note.
func (r *Reflector) AddImprovement(imp Improvement) {
	if imp.Time.IsZero() {
		imp.Time = time.Now()
	}
	r.improvements = append(r.improvements, imp)

	// Keep last 50 improvements
	if len(r.improvements) > 50 {
		r.improvements = r.improvements[len(r.improvements)-50:]
	}

	r.persist()
}

// GetLearnings returns recent learnings.
func (r *Reflector) GetLearnings(limit int) []Learning {
	if limit <= 0 || limit > len(r.learnings) {
		return r.learnings
	}
	return r.learnings[len(r.learnings)-limit:]
}

// GetImprovements returns improvement notes.
func (r *Reflector) GetImprovements(unappliedOnly bool) []Improvement {
	if !unappliedOnly {
		return r.improvements
	}

	var unapplied []Improvement
	for _, imp := range r.improvements {
		if !imp.Applied {
			unapplied = append(unapplied, imp)
		}
	}
	return unapplied
}

// MarkApplied marks an improvement as applied.
func (r *Reflector) MarkApplied(idx int) {
	if idx >= 0 && idx < len(r.improvements) {
		r.improvements[idx].Applied = true
		r.persist()
	}
}

// GeneratePromptAdditions generates additions to Tony's system prompt
// based on learnings and improvements.
func (r *Reflector) GeneratePromptAdditions() string {
	var additions strings.Builder

	// Add recent learnings
	recentLearnings := r.GetLearnings(5)
	if len(recentLearnings) > 0 {
		additions.WriteString("\n## Recent Learnings\n")
		for _, l := range recentLearnings {
			additions.WriteString(fmt.Sprintf("- %s\n", l.Content))
		}
	}

	// Add unapplied improvements
	unapplied := r.GetImprovements(true)
	if len(unapplied) > 0 {
		additions.WriteString("\n## Self-Improvement Notes\n")
		for _, imp := range unapplied {
			additions.WriteString(fmt.Sprintf("- %s: %s → %s\n",
				imp.Area, imp.Observation, imp.Action))
		}
	}

	return additions.String()
}

// persist saves state to disk.
func (r *Reflector) persist() {
	dir := filepath.Join(r.baseDir, "life")
	os.MkdirAll(dir, 0755)

	data := struct {
		Learnings    []Learning    `json:"learnings"`
		Improvements []Improvement `json:"improvements"`
	}{
		Learnings:    r.learnings,
		Improvements: r.improvements,
	}

	jsonData, _ := json.MarshalIndent(data, "", "  ")
	os.WriteFile(filepath.Join(dir, "reflection.json"), jsonData, 0644)
}

// load restores state from disk.
func (r *Reflector) load() {
	path := filepath.Join(r.baseDir, "life", "reflection.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var state struct {
		Learnings    []Learning    `json:"learnings"`
		Improvements []Improvement `json:"improvements"`
	}

	if err := json.Unmarshal(data, &state); err == nil {
		r.learnings = state.Learnings
		r.improvements = state.Improvements
	}
}
