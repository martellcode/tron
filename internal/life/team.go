package life

import (
	"context"
	"fmt"
	"strings"

	"github.com/martellcode/vega"
)

// TeamChecker monitors spawned agents and their work.
type TeamChecker struct {
	orch *vega.Orchestrator
}

// NewTeamChecker creates a new team checker.
func NewTeamChecker(orch *vega.Orchestrator) *TeamChecker {
	return &TeamChecker{
		orch: orch,
	}
}

// Check reviews the status of all team members (spawned processes).
func (t *TeamChecker) Check(ctx context.Context) string {
	if t.orch == nil {
		return ""
	}

	processes := t.orch.List()
	if len(processes) == 0 {
		return "No active team members at the moment."
	}

	var status strings.Builder
	status.WriteString(fmt.Sprintf("Team status (%d active): ", len(processes)))

	running := 0
	completed := 0
	failed := 0

	for _, p := range processes {
		switch p.Status() {
		case vega.StatusRunning:
			running++
			if p.Agent != nil {
				status.WriteString(fmt.Sprintf("%s is working. ", p.Agent.Name))
			}
		case vega.StatusCompleted:
			completed++
		case vega.StatusFailed:
			failed++
			if p.Agent != nil {
				status.WriteString(fmt.Sprintf("%s failed - needs attention. ", p.Agent.Name))
			}
		}
	}

	if running == 0 && completed > 0 {
		status.WriteString(fmt.Sprintf("%d completed their work. ", completed))
	}
	if failed > 0 {
		status.WriteString(fmt.Sprintf("%d failed - investigating. ", failed))
	}

	return status.String()
}

// GetActiveAgents returns a list of currently active agent names.
func (t *TeamChecker) GetActiveAgents() []string {
	if t.orch == nil {
		return nil
	}

	processes := t.orch.List()
	var active []string

	for _, p := range processes {
		if p.Status() == vega.StatusRunning && p.Agent != nil {
			active = append(active, p.Agent.Name)
		}
	}

	return active
}

// GetAgentStatus returns detailed status for a specific agent.
func (t *TeamChecker) GetAgentStatus(name string) string {
	if t.orch == nil {
		return "Orchestrator not available"
	}

	processes := t.orch.List()
	for _, p := range processes {
		if p.Agent != nil && p.Agent.Name == name {
			metrics := p.Metrics()
			return fmt.Sprintf("%s: status=%s, iterations=%d, cost=$%.4f",
				name,
				p.Status(),
				metrics.Iterations,
				metrics.CostUSD,
			)
		}
	}

	return fmt.Sprintf("%s is not currently active", name)
}
