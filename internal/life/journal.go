package life

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Journal manages Tony's daily thoughts and observations.
type Journal struct {
	baseDir string

	mu      sync.RWMutex
	entries []JournalEntry
}

// JournalEntry represents a single journal entry.
type JournalEntry struct {
	Time     time.Time         `json:"time"`
	Type     string            `json:"type"` // reading, goals, team, reflection, tweet, thought
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// NewJournal creates a new journal.
func NewJournal(baseDir string) *Journal {
	j := &Journal{
		baseDir: baseDir,
		entries: make([]JournalEntry, 0),
	}
	j.load()
	return j
}

// Add appends a new journal entry.
func (j *Journal) Add(entry JournalEntry) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}

	j.entries = append(j.entries, entry)

	// Keep only last 30 days of entries
	cutoff := time.Now().AddDate(0, 0, -30)
	var kept []JournalEntry
	for _, e := range j.entries {
		if e.Time.After(cutoff) {
			kept = append(kept, e)
		}
	}
	j.entries = kept

	j.persist()

	// Also write to daily log file for easy reading
	j.appendToDaily(entry)
}

// Recent returns entries from the specified duration.
func (j *Journal) Recent(within time.Duration) []JournalEntry {
	j.mu.RLock()
	defer j.mu.RUnlock()

	cutoff := time.Now().Add(-within)
	var recent []JournalEntry
	for _, e := range j.entries {
		if e.Time.After(cutoff) {
			recent = append(recent, e)
		}
	}
	return recent
}

// Today returns all entries from today.
func (j *Journal) Today() []JournalEntry {
	j.mu.RLock()
	defer j.mu.RUnlock()

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	var today []JournalEntry
	for _, e := range j.entries {
		if e.Time.After(startOfDay) {
			today = append(today, e)
		}
	}
	return today
}

// Summarize creates a summary entry for the current period.
func (j *Journal) Summarize(ctx context.Context) string {
	today := j.Today()
	if len(today) == 0 {
		return ""
	}

	// Count activities by type
	counts := make(map[string]int)
	for _, e := range today {
		counts[e.Type]++
	}

	summary := "Day summary: "
	if counts["reading"] > 0 {
		summary += fmt.Sprintf("Read %d articles. ", counts["reading"])
	}
	if counts["team"] > 0 {
		summary += fmt.Sprintf("Checked on team %d times. ", counts["team"])
	}
	if counts["reflection"] > 0 {
		summary += fmt.Sprintf("Reflected %d times. ", counts["reflection"])
	}
	if counts["post"] > 0 {
		summary += fmt.Sprintf("Posted %d times. ", counts["post"])
	}

	j.Add(JournalEntry{
		Time:    time.Now(),
		Type:    "summary",
		Content: summary,
	})

	return summary
}

// GetForDisplay returns journal entries formatted for display.
func (j *Journal) GetForDisplay(limit int) string {
	j.mu.RLock()
	defer j.mu.RUnlock()

	if limit <= 0 || limit > len(j.entries) {
		limit = len(j.entries)
	}

	// Get most recent entries
	start := len(j.entries) - limit
	if start < 0 {
		start = 0
	}

	var output string
	for _, e := range j.entries[start:] {
		output += fmt.Sprintf("[%s] %s: %s\n",
			e.Time.Format("2006-01-02 15:04"),
			e.Type,
			e.Content,
		)
	}

	return output
}

// appendToDaily writes entry to a daily log file for easy reading.
func (j *Journal) appendToDaily(entry JournalEntry) {
	dir := filepath.Join(j.baseDir, "life", "journal")
	os.MkdirAll(dir, 0755)

	filename := entry.Time.Format("2006-01-02") + ".md"
	path := filepath.Join(dir, filename)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	line := fmt.Sprintf("**%s** [%s] %s\n\n",
		entry.Time.Format("15:04"),
		entry.Type,
		entry.Content,
	)
	f.WriteString(line)
}

// persist saves all entries to disk.
func (j *Journal) persist() {
	dir := filepath.Join(j.baseDir, "life")
	os.MkdirAll(dir, 0755)

	data, err := json.MarshalIndent(j.entries, "", "  ")
	if err != nil {
		return
	}

	os.WriteFile(filepath.Join(dir, "journal.json"), data, 0644)
}

// load restores entries from disk.
func (j *Journal) load() {
	path := filepath.Join(j.baseDir, "life", "journal.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	json.Unmarshal(data, &j.entries)
}
