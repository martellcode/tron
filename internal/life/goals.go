package life

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GoalTracker manages company goals and Tony's contemplation of them.
type GoalTracker struct {
	baseDir string
	goals   Goals
}

// Goals represents the company's goals and priorities.
type Goals struct {
	Mission    string   `yaml:"mission"`
	Vision     string   `yaml:"vision"`
	Quarter    string   `yaml:"quarter"`
	Objectives []OKR    `yaml:"objectives"`
	Projects   []Project `yaml:"projects"`
	Values     []string `yaml:"values"`
}

// OKR represents an Objective and Key Results.
type OKR struct {
	Objective  string      `yaml:"objective"`
	KeyResults []KeyResult `yaml:"key_results"`
	Owner      string      `yaml:"owner,omitempty"`
	DueDate    string      `yaml:"due_date,omitempty"`
}

// KeyResult represents a measurable key result.
type KeyResult struct {
	Description string  `yaml:"description"`
	Target      float64 `yaml:"target"`
	Current     float64 `yaml:"current"`
	Unit        string  `yaml:"unit,omitempty"`
}

// Project represents an active project.
type Project struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Status      string   `yaml:"status"` // active, paused, completed
	Priority    string   `yaml:"priority"` // high, medium, low
	Owner       string   `yaml:"owner,omitempty"`
	DueDate     string   `yaml:"due_date,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
}

// NewGoalTracker creates a new goal tracker.
func NewGoalTracker(baseDir string) *GoalTracker {
	gt := &GoalTracker{
		baseDir: baseDir,
	}
	gt.load()
	return gt
}

// Contemplate reviews goals and generates thoughts about them.
func (g *GoalTracker) Contemplate(ctx context.Context) string {
	if g.goals.Mission == "" && len(g.goals.Objectives) == 0 {
		return "No goals defined yet. Should set up company goals and OKRs."
	}

	var thoughts strings.Builder

	// Check OKR progress
	for _, okr := range g.goals.Objectives {
		for _, kr := range okr.KeyResults {
			if kr.Target > 0 {
				progress := (kr.Current / kr.Target) * 100
				if progress < 50 {
					thoughts.WriteString(fmt.Sprintf("Behind on '%s' - %s (%.0f%% complete). ",
						okr.Objective, kr.Description, progress))
				} else if progress >= 100 {
					thoughts.WriteString(fmt.Sprintf("Completed: %s. ", kr.Description))
				}
			}
		}
	}

	// Check project statuses
	var activeHighPriority []string
	var stalled []string
	for _, p := range g.goals.Projects {
		if p.Status == "active" && p.Priority == "high" {
			activeHighPriority = append(activeHighPriority, p.Name)
		}
		if p.Status == "paused" {
			stalled = append(stalled, p.Name)
		}
	}

	if len(activeHighPriority) > 0 {
		thoughts.WriteString(fmt.Sprintf("High priority active: %s. ", strings.Join(activeHighPriority, ", ")))
	}
	if len(stalled) > 0 {
		thoughts.WriteString(fmt.Sprintf("Stalled projects need attention: %s. ", strings.Join(stalled, ", ")))
	}

	// Check for upcoming due dates
	now := time.Now()
	for _, p := range g.goals.Projects {
		if p.DueDate != "" {
			if due, err := time.Parse("2006-01-02", p.DueDate); err == nil {
				daysUntil := int(due.Sub(now).Hours() / 24)
				if daysUntil >= 0 && daysUntil <= 7 {
					thoughts.WriteString(fmt.Sprintf("'%s' due in %d days. ", p.Name, daysUntil))
				}
			}
		}
	}

	result := thoughts.String()
	if result == "" {
		result = "Goals review: on track, no immediate concerns."
	}

	return result
}

// GetGoals returns the current goals.
func (g *GoalTracker) GetGoals() Goals {
	return g.goals
}

// UpdateProject updates a project's status.
func (g *GoalTracker) UpdateProject(name, status string) {
	for i, p := range g.goals.Projects {
		if strings.EqualFold(p.Name, name) {
			g.goals.Projects[i].Status = status
			g.persist()
			return
		}
	}
}

// load reads goals from disk.
func (g *GoalTracker) load() {
	// Try multiple locations
	paths := []string{
		filepath.Join(g.baseDir, "goals.yaml"),
		filepath.Join(g.baseDir, "knowledge", "goals.yaml"),
		filepath.Join(g.baseDir, "life", "goals.yaml"),
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if err := yaml.Unmarshal(data, &g.goals); err == nil {
			return
		}
	}

	// Create default goals file if none exists
	g.createDefault()
}

// persist saves goals to disk.
func (g *GoalTracker) persist() {
	dir := filepath.Join(g.baseDir, "life")
	os.MkdirAll(dir, 0755)

	data, err := yaml.Marshal(g.goals)
	if err != nil {
		return
	}

	os.WriteFile(filepath.Join(dir, "goals.yaml"), data, 0644)
}

// createDefault creates a default goals file.
func (g *GoalTracker) createDefault() {
	g.goals = Goals{
		Mission: "Build intelligent systems that amplify human potential",
		Vision:  "A world where AI handles the tedious so humans can focus on the creative",
		Quarter: time.Now().Format("2006-Q1"),
		Values: []string{
			"Ship fast, iterate faster",
			"Simplicity over complexity",
			"Users first",
			"Transparent by default",
		},
		Objectives: []OKR{
			{
				Objective: "Launch Vega to the public",
				KeyResults: []KeyResult{
					{Description: "Open source release", Target: 1, Current: 0, Unit: "release"},
					{Description: "GitHub stars", Target: 100, Current: 0, Unit: "stars"},
					{Description: "Documentation pages", Target: 10, Current: 8, Unit: "pages"},
				},
			},
		},
		Projects: []Project{
			{
				Name:        "Vega Open Source",
				Description: "Prepare and release vega as open source",
				Status:      "active",
				Priority:    "high",
			},
			{
				Name:        "Tony Life Loop",
				Description: "Give Tony autonomous daily routines",
				Status:      "active",
				Priority:    "high",
			},
		},
	}

	g.persist()
}
