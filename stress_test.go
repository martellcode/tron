package tron_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vegaops/vega"
	"github.com/vegaops/vega/llm"
)

// =============================================================================
// MOCK LLM FOR FREE STRESS TESTING
// =============================================================================

// stressLLM simulates realistic LLM behavior with configurable latency
type stressLLM struct {
	mu            sync.Mutex
	callCount     int64
	totalLatency  time.Duration
	minLatency    time.Duration
	maxLatency    time.Duration
	errorRate     float64 // 0.0 - 1.0
	responseSize  int     // approximate tokens
}

func newStressLLM(minLatency, maxLatency time.Duration, errorRate float64) *stressLLM {
	return &stressLLM{
		minLatency:   minLatency,
		maxLatency:   maxLatency,
		errorRate:    errorRate,
		responseSize: 100,
	}
}

func (m *stressLLM) simulateLatency() {
	if m.maxLatency > m.minLatency {
		jitter := time.Duration(rand.Int63n(int64(m.maxLatency - m.minLatency)))
		time.Sleep(m.minLatency + jitter)
	} else {
		time.Sleep(m.minLatency)
	}
}

func (m *stressLLM) Generate(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (*vega.LLMResponse, error) {
	atomic.AddInt64(&m.callCount, 1)
	start := time.Now()

	// Simulate latency
	m.simulateLatency()

	m.mu.Lock()
	m.totalLatency += time.Since(start)
	m.mu.Unlock()

	// Simulate random errors
	if m.errorRate > 0 && rand.Float64() < m.errorRate {
		return nil, fmt.Errorf("simulated LLM error")
	}

	// Check context cancellation
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	return &vega.LLMResponse{
		Content:      fmt.Sprintf("Stress test response %d", atomic.LoadInt64(&m.callCount)),
		InputTokens:  100,
		OutputTokens: m.responseSize,
		CostUSD:      0.001,
	}, nil
}

func (m *stressLLM) GenerateStream(ctx context.Context, messages []vega.Message, tools []vega.ToolSchema) (<-chan vega.StreamEvent, error) {
	atomic.AddInt64(&m.callCount, 1)

	ch := make(chan vega.StreamEvent, 20)
	go func() {
		defer close(ch)

		// Simulate latency spread across chunks
		chunkDelay := m.minLatency / 10

		ch <- vega.StreamEvent{Type: vega.StreamEventMessageStart}
		ch <- vega.StreamEvent{Type: vega.StreamEventContentStart}

		words := []string{"Stress", "test", "streaming", "response"}
		for _, word := range words {
			select {
			case <-ctx.Done():
				ch <- vega.StreamEvent{Type: vega.StreamEventError, Error: ctx.Err()}
				return
			default:
			}
			time.Sleep(chunkDelay)
			ch <- vega.StreamEvent{Type: vega.StreamEventContentDelta, Delta: word + " "}
		}

		ch <- vega.StreamEvent{Type: vega.StreamEventContentEnd}
		ch <- vega.StreamEvent{Type: vega.StreamEventMessageEnd}
	}()

	return ch, nil
}

func (m *stressLLM) Stats() (calls int64, avgLatency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls = atomic.LoadInt64(&m.callCount)
	if calls > 0 {
		avgLatency = m.totalLatency / time.Duration(calls)
	}
	return
}

// =============================================================================
// STRESS TESTS (FREE - MOCK BASED)
// =============================================================================

func TestStress_ManyProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	llm := newStressLLM(1*time.Millisecond, 5*time.Millisecond, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(500),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "StressAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("You are a stress test agent."),
		Tools:  vega.NewTools(),
	}

	numProcesses := 100
	processes := make([]*vega.Process, 0, numProcesses)

	// Spawn many processes
	start := time.Now()
	for i := 0; i < numProcesses; i++ {
		proc, err := orch.Spawn(agent)
		if err != nil {
			t.Fatalf("Failed to spawn process %d: %v", i, err)
		}
		processes = append(processes, proc)
	}
	spawnTime := time.Since(start)

	t.Logf("Spawned %d processes in %v (%.2f/sec)", numProcesses, spawnTime, float64(numProcesses)/spawnTime.Seconds())

	// Verify all running
	running := 0
	for _, p := range processes {
		if p.Status() == vega.StatusRunning {
			running++
		}
	}
	if running != numProcesses {
		t.Errorf("Running processes = %d, want %d", running, numProcesses)
	}

	// Clean up
	for _, p := range processes {
		p.Stop()
	}
}

func TestStress_ConcurrentMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	llm := newStressLLM(5*time.Millisecond, 20*time.Millisecond, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(50),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "ConcurrentAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("You handle concurrent requests."),
		Tools:  vega.NewTools(),
	}

	numProcesses := 10
	messagesPerProcess := 20
	processes := make([]*vega.Process, numProcesses)

	for i := 0; i < numProcesses; i++ {
		proc, err := orch.Spawn(agent)
		if err != nil {
			t.Fatalf("Failed to spawn: %v", err)
		}
		processes[i] = proc
	}

	var wg sync.WaitGroup
	var successCount, errorCount int64
	ctx := context.Background()

	start := time.Now()

	// Send messages concurrently to all processes
	for _, proc := range processes {
		for j := 0; j < messagesPerProcess; j++ {
			wg.Add(1)
			go func(p *vega.Process, msgNum int) {
				defer wg.Done()
				_, err := p.Send(ctx, fmt.Sprintf("Message %d", msgNum))
				if err != nil {
					atomic.AddInt64(&errorCount, 1)
				} else {
					atomic.AddInt64(&successCount, 1)
				}
			}(proc, j)
		}
	}

	wg.Wait()
	duration := time.Since(start)

	totalMessages := numProcesses * messagesPerProcess
	calls, avgLatency := llm.Stats()

	t.Logf("Sent %d messages in %v", totalMessages, duration)
	t.Logf("Success: %d, Errors: %d", successCount, errorCount)
	t.Logf("LLM calls: %d, Avg latency: %v", calls, avgLatency)
	t.Logf("Throughput: %.2f msg/sec", float64(successCount)/duration.Seconds())

	if errorCount > 0 {
		t.Errorf("Had %d errors", errorCount)
	}
}

func TestStress_RapidSpawnKill(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	llm := newStressLLM(1*time.Millisecond, 2*time.Millisecond, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(100),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "EphemeralAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Short-lived agent."),
		Tools:  vega.NewTools(),
	}

	iterations := 200
	var spawnErrors, killErrors int64

	start := time.Now()

	for i := 0; i < iterations; i++ {
		proc, err := orch.Spawn(agent)
		if err != nil {
			atomic.AddInt64(&spawnErrors, 1)
			continue
		}

		// Immediately kill
		if err := orch.Kill(proc.ID); err != nil {
			atomic.AddInt64(&killErrors, 1)
		}
	}

	duration := time.Since(start)

	t.Logf("Spawn/kill cycles: %d in %v", iterations, duration)
	t.Logf("Spawn errors: %d, Kill errors: %d", spawnErrors, killErrors)
	t.Logf("Rate: %.2f cycles/sec", float64(iterations)/duration.Seconds())

	// Should have no lingering processes
	procs := orch.List()
	if len(procs) > 0 {
		t.Errorf("Lingering processes: %d", len(procs))
	}
}

func TestStress_StreamingUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	llm := newStressLLM(10*time.Millisecond, 50*time.Millisecond, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(50),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "StreamAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Streaming agent."),
		Tools:  vega.NewTools(),
	}

	numStreams := 20
	var wg sync.WaitGroup
	var successCount, errorCount int64
	ctx := context.Background()

	start := time.Now()

	for i := 0; i < numStreams; i++ {
		wg.Add(1)
		go func(streamNum int) {
			defer wg.Done()

			proc, err := orch.Spawn(agent)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer proc.Stop()

			stream, err := proc.SendStream(ctx, "Stream request")
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}

			// Consume stream
			var chunks int
			for range stream.Chunks() {
				chunks++
			}

			if stream.Err() != nil {
				atomic.AddInt64(&errorCount, 1)
			} else {
				atomic.AddInt64(&successCount, 1)
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	t.Logf("Concurrent streams: %d in %v", numStreams, duration)
	t.Logf("Success: %d, Errors: %d", successCount, errorCount)

	if errorCount > 0 {
		t.Errorf("Had %d streaming errors", errorCount)
	}
}

func TestStress_ErrorRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// 10% error rate
	llm := newStressLLM(5*time.Millisecond, 10*time.Millisecond, 0.1)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(50),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "ResilientAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Handles errors gracefully."),
		Tools:  vega.NewTools(),
	}

	proc, err := orch.Spawn(agent, vega.WithSupervision(vega.Supervision{
		Strategy:    vega.Restart,
		MaxRestarts: 5,
	}))
	if err != nil {
		t.Fatalf("Failed to spawn: %v", err)
	}
	defer proc.Stop()

	ctx := context.Background()
	numRequests := 50
	var successCount, errorCount int64

	for i := 0; i < numRequests; i++ {
		_, err := proc.Send(ctx, fmt.Sprintf("Request %d", i))
		if err != nil {
			atomic.AddInt64(&errorCount, 1)
		} else {
			atomic.AddInt64(&successCount, 1)
		}
	}

	t.Logf("Requests: %d, Success: %d, Errors: %d", numRequests, successCount, errorCount)
	t.Logf("Success rate: %.1f%%", float64(successCount)/float64(numRequests)*100)

	// With 10% error rate, expect ~90% success
	expectedMin := int64(float64(numRequests) * 0.85) // Allow some variance
	if successCount < expectedMin {
		t.Errorf("Success count %d below expected minimum %d", successCount, expectedMin)
	}
}

func TestStress_MemoryStability(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	llm := newStressLLM(1*time.Millisecond, 2*time.Millisecond, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(200),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "MemoryAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Testing memory stability."),
		Tools:  vega.NewTools(),
	}

	// Force GC and get baseline
	runtime.GC()
	var baselineStats runtime.MemStats
	runtime.ReadMemStats(&baselineStats)

	ctx := context.Background()
	iterations := 100

	for i := 0; i < iterations; i++ {
		proc, err := orch.Spawn(agent)
		if err != nil {
			continue
		}

		// Send a few messages
		for j := 0; j < 5; j++ {
			proc.Send(ctx, "Test message")
		}

		proc.Stop()
		orch.Kill(proc.ID)

		// Periodic GC
		if i%20 == 0 {
			runtime.GC()
		}
	}

	// Final GC and measure
	runtime.GC()
	var finalStats runtime.MemStats
	runtime.ReadMemStats(&finalStats)

	heapGrowth := int64(finalStats.HeapAlloc) - int64(baselineStats.HeapAlloc)
	heapGrowthMB := float64(heapGrowth) / 1024 / 1024

	t.Logf("Iterations: %d", iterations)
	t.Logf("Baseline heap: %.2f MB", float64(baselineStats.HeapAlloc)/1024/1024)
	t.Logf("Final heap: %.2f MB", float64(finalStats.HeapAlloc)/1024/1024)
	t.Logf("Heap growth: %.2f MB", heapGrowthMB)
	t.Logf("Goroutines: %d", runtime.NumGoroutine())

	// Warn if significant growth (not fail - some growth is normal)
	if heapGrowthMB > 50 {
		t.Logf("WARNING: Significant heap growth detected")
	}
}

func TestStress_GoroutineLeaks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Get baseline goroutine count
	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()

	llm := newStressLLM(1*time.Millisecond, 2*time.Millisecond, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(100),
	)

	agent := vega.Agent{
		Name:   "LeakTestAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Checking for leaks."),
		Tools:  vega.NewTools(),
	}

	ctx := context.Background()

	// Spawn, use, and kill many processes
	for i := 0; i < 50; i++ {
		proc, err := orch.Spawn(agent)
		if err != nil {
			continue
		}
		proc.Send(ctx, "Test")
		proc.Stop()
	}

	// Shutdown orchestrator
	orch.Shutdown(context.Background())

	// Allow goroutines to clean up
	time.Sleep(100 * time.Millisecond)
	runtime.GC()

	finalGoroutines := runtime.NumGoroutine()
	leaked := finalGoroutines - baselineGoroutines

	t.Logf("Baseline goroutines: %d", baselineGoroutines)
	t.Logf("Final goroutines: %d", finalGoroutines)
	t.Logf("Potential leaks: %d", leaked)

	// Allow small variance (background runtime goroutines)
	if leaked > 5 {
		t.Errorf("Potential goroutine leak: %d extra goroutines", leaked)
	}
}

// =============================================================================
// REAL API STRESS TEST (EXPENSIVE - USE WITH CAUTION)
// =============================================================================

func TestStress_RealAPI(t *testing.T) {
	// Only run if explicitly enabled and API key is set
	if os.Getenv("STRESS_TEST_REAL_API") != "true" {
		t.Skip("Skipping real API stress test. Set STRESS_TEST_REAL_API=true to enable.")
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	// WARNING: This will cost money!
	// Estimated cost for this test: ~$0.10-0.50 depending on response lengths
	t.Log("WARNING: This test makes real API calls and costs money!")

	anthropic := llm.NewAnthropic(llm.WithAPIKey(apiKey))
	orch := vega.NewOrchestrator(
		vega.WithLLM(anthropic),
		vega.WithMaxProcesses(10),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "RealAPIAgent",
		Model:  "claude-sonnet-4-20250514",
		System: vega.StaticPrompt("You are a test agent. Respond briefly with just 'OK' to any message."),
		Tools:  vega.NewTools(),
	}

	// Small scale - just 5 concurrent requests
	numRequests := 5
	var wg sync.WaitGroup
	var successCount, errorCount int64
	var totalCost float64
	var costMu sync.Mutex

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	start := time.Now()

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(reqNum int) {
			defer wg.Done()

			proc, err := orch.Spawn(agent)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				t.Logf("Spawn error: %v", err)
				return
			}
			defer proc.Stop()

			resp, err := proc.Send(ctx, "Say OK")
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				t.Logf("Send error: %v", err)
				return
			}

			atomic.AddInt64(&successCount, 1)
			metrics := proc.Metrics()

			costMu.Lock()
			totalCost += metrics.CostUSD
			costMu.Unlock()

			t.Logf("Request %d: %q (cost: $%.4f)", reqNum, resp, metrics.CostUSD)
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	t.Logf("=== Real API Stress Test Results ===")
	t.Logf("Requests: %d, Success: %d, Errors: %d", numRequests, successCount, errorCount)
	t.Logf("Duration: %v", duration)
	t.Logf("Total cost: $%.4f", totalCost)

	if errorCount > 0 {
		t.Errorf("Had %d errors in real API test", errorCount)
	}
}

// =============================================================================
// BENCHMARK TESTS
// =============================================================================

func BenchmarkProcessSpawn(b *testing.B) {
	llm := newStressLLM(0, 0, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(b.N+100),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "BenchAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Benchmark agent."),
		Tools:  vega.NewTools(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		proc, _ := orch.Spawn(agent)
		proc.Stop()
	}
}

func BenchmarkProcessSend(b *testing.B) {
	llm := newStressLLM(0, 0, 0)
	orch := vega.NewOrchestrator(vega.WithLLM(llm))
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "BenchAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Benchmark agent."),
		Tools:  vega.NewTools(),
	}

	proc, _ := orch.Spawn(agent)
	defer proc.Stop()

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		proc.Send(ctx, "Benchmark message")
	}
}

func BenchmarkConcurrentSend(b *testing.B) {
	llm := newStressLLM(0, 0, 0)
	orch := vega.NewOrchestrator(
		vega.WithLLM(llm),
		vega.WithMaxProcesses(100),
	)
	defer orch.Shutdown(context.Background())

	agent := vega.Agent{
		Name:   "BenchAgent",
		Model:  "test-model",
		System: vega.StaticPrompt("Benchmark agent."),
		Tools:  vega.NewTools(),
	}

	// Create a pool of processes
	numProcs := 10
	procs := make([]*vega.Process, numProcs)
	for i := 0; i < numProcs; i++ {
		procs[i], _ = orch.Spawn(agent)
	}
	defer func() {
		for _, p := range procs {
			p.Stop()
		}
	}()

	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			proc := procs[i%numProcs]
			proc.Send(ctx, "Benchmark message")
			i++
		}
	})
}
