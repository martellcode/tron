package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/martellcode/tron/internal/callback"
	"github.com/martellcode/tron/internal/config"
	"github.com/martellcode/tron/internal/email"
	"github.com/martellcode/tron/internal/life"
	"github.com/martellcode/tron/internal/server"
	"github.com/martellcode/tron/internal/slack"
	"github.com/martellcode/tron/internal/tools"
	"github.com/martellcode/tron/internal/vapi"
	"github.com/martellcode/tron/internal/voice/elevenlabs"
	"github.com/martellcode/vega"
	"github.com/martellcode/vega/container"
	"github.com/martellcode/vega/dsl"
	"github.com/martellcode/vega/llm"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "chat":
		runChat(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	tronDir := config.DefaultTronDir()
	fmt.Printf(`Tron - Tony's AI Team

Usage:
  tron <command> [options]

Commands:
  serve    Start the HTTP server for VAPI webhooks
  chat     Interactive CLI chat with Tony
  help     Show this help message

Options:
  serve:
    -port      Port to listen on (default: from TRON_PORT env or 3000)
    -config    Path to vega config (default: %s/tron.vega.yaml)

  chat:
    -agent     Agent to chat with (default: first agent in config)
    -config    Path to vega config (default: %s/tron.vega.yaml)

Configuration:
  Tron reads configuration from %s
  Set WORKING_DIR and AGENTS_DIR in %s/config
`, tronDir, tronDir, tronDir, tronDir)
}

func runServe(args []string) {
	// Load tron config first (this loads .env files)
	tronCfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load tron config: %v", err)
	}

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", tronCfg.Port, "Port to listen on")
	configPath := fs.String("config", tronCfg.ConfigFile, "Path to vega config")
	fs.Parse(args)

	// Check for API key
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY environment variable required")
	}

	log.Printf("Using config: %s", *configPath)
	log.Printf("Working directory: %s", tronCfg.WorkingDir)
	log.Printf("Agents directory: %s", tronCfg.AgentsDir)

	// Load vega config
	parser := dsl.NewParser()
	cfg, err := parser.ParseFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create LLM backend
	anthropic := llm.NewAnthropic(
		llm.WithAPIKey(apiKey),
	)

	// Create container manager for project isolation
	cm, err := container.NewManager(tronCfg.WorkingDir)
	if err != nil {
		log.Printf("Warning: Container manager failed to initialize: %v", err)
	} else if cm.IsAvailable() {
		log.Printf("Docker container manager initialized")
	} else {
		log.Printf("Docker not available, running in direct mode")
	}

	// Create orchestrator with container support
	orch := vega.NewOrchestrator(
		vega.WithLLM(anthropic),
		vega.WithMaxProcesses(50),
		vega.WithRecovery(true),
		vega.WithContainerManager(cm, tronCfg.WorkingDir),
	)

	// Register custom tools with container support
	customTools := tools.NewPersonaTools(orch, cfg, tronCfg.WorkingDir, tronCfg.TronDir, cm)

	// Create and start server
	srv := server.New(orch, cfg, customTools, *port, tronCfg.WorkingDir)
	srv.SetBaseDir(tronCfg.TronDir)

	// Wire up process manager for subdomain routing
	customTools.SetProcessManager(srv.GetProcessManager())
	log.Printf("Subdomain routing enabled (*.hellotron.com)")

	// Initialize VAPI client if configured
	vapiAPIKey := os.Getenv("VAPI_API_KEY")
	vapiPhoneID := os.Getenv("VAPI_PHONE_NUMBER_ID")
	vapiAssistantID := os.Getenv("VAPI_ASSISTANT_ID")
	var vapiClient *vapi.Client
	if vapiAPIKey != "" && vapiPhoneID != "" {
		vapiClient = vapi.NewClient(vapiAPIKey, vapiPhoneID, vapiAssistantID)
		log.Printf("VAPI integration enabled")
	}

	// Initialize ElevenLabs client if configured
	elevenLabsAPIKey := os.Getenv("ELEVENLABS_API_KEY")
	elevenLabsAgentID := os.Getenv("ELEVENLABS_AGENT_ID")
	if elevenLabsAPIKey != "" && elevenLabsAgentID != "" {
		elClient := elevenlabs.NewClient(elevenLabsAPIKey, elevenLabsAgentID)
		srv.SetElevenLabsClient(elClient)
		log.Printf("ElevenLabs integration enabled")
	}

	// Initialize email client if configured
	var emailClient *email.Client
	smtpHost := os.Getenv("SMTP_HOST")
	smtpFrom := os.Getenv("SMTP_FROM")
	if smtpHost != "" && smtpFrom != "" {
		smtpPort := 587
		if portStr := os.Getenv("SMTP_PORT"); portStr != "" {
			if p, err := strconv.Atoi(portStr); err == nil {
				smtpPort = p
			}
		}
		emailClient = email.NewClient(
			smtpHost,
			smtpPort,
			os.Getenv("SMTP_USER"),
			os.Getenv("SMTP_PASSWORD"),
			smtpFrom,
		)
		log.Printf("Email notifications enabled")
	}

	// Initialize callback registry
	callbackRegistry := callback.NewRegistry(vapiClient, emailClient, tronCfg.TronDir, "Tony", smtpFrom)
	srv.SetCallbackRegistry(callbackRegistry)

	// Initialize Slack handler if configured
	slackBotToken := os.Getenv("SLACK_BOT_TOKEN")
	slackSigningSecret := os.Getenv("SLACK_SIGNING_SECRET")
	var slackClient *slack.Client
	if slackBotToken != "" {
		slackClient = slack.NewClient(slackBotToken)
		slackHandler := slack.NewHandler(slackClient, slackSigningSecret, orch, cfg, tronCfg.TronDir)
		srv.SetSlackHandler(slackHandler)
		log.Printf("Slack integration enabled")
	}

	// Initialize Tony's life loop (autonomous daily routine)
	lifeConfig := life.DefaultConfig(tronCfg.TronDir)
	if apiURL := os.Getenv("TRON_SOCIAL_API_URL"); apiURL != "" {
		lifeConfig.SocialEnabled = true
		lifeConfig.SocialAPIURL = apiURL
		lifeConfig.SocialAPIKey = os.Getenv("TRON_SOCIAL_API_KEY")
	}
	if slackChannel := os.Getenv("TRON_LIFE_SLACK_CHANNEL"); slackChannel != "" {
		lifeConfig.SlackChannel = slackChannel
	}
	tonyLife := life.New(orch, lifeConfig)
	if slackClient != nil && lifeConfig.SlackChannel != "" {
		tonyLife.SetSlack(slackClient)
		log.Printf("Tony's life updates will post to Slack channel: %s", lifeConfig.SlackChannel)
	}
	srv.SetLifeLoop(tonyLife)
	tonyLife.Start()
	log.Printf("Tony's life loop started")

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
		tonyLife.Stop()
		srv.Shutdown(ctx)
		orch.Shutdown(ctx)
	}()

	log.Printf("Tron server starting on port %d", *port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func runChat(args []string) {
	// Load tron config first (this loads .env files)
	tronCfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load tron config: %v", err)
	}

	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	configPath := fs.String("config", tronCfg.ConfigFile, "Path to vega config")
	agentName := fs.String("agent", "", "Agent to chat with (default: first agent in config)")
	fs.Parse(args)

	// Check for API key
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY environment variable required")
	}

	// Load vega config
	parser := dsl.NewParser()
	cfg, err := parser.ParseFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Determine which agent to use
	selectedAgent := *agentName
	if selectedAgent == "" {
		// Use first agent in config
		for name := range cfg.Agents {
			selectedAgent = name
			break
		}
	}

	// Get agent definition
	agentDef, ok := cfg.Agents[selectedAgent]
	if !ok {
		log.Fatalf("Agent %q not found in config. Available agents:", selectedAgent)
		for name := range cfg.Agents {
			log.Printf("  - %s", name)
		}
		os.Exit(1)
	}

	log.Printf("Using config: %s", *configPath)
	log.Printf("Chatting with: %s", selectedAgent)

	// Create LLM backend
	anthropic := llm.NewAnthropic(
		llm.WithAPIKey(apiKey),
	)

	// Create container manager for project isolation
	cm, err := container.NewManager(tronCfg.WorkingDir)
	if err != nil {
		log.Printf("Warning: Container manager failed to initialize: %v", err)
	}

	// Create orchestrator with container support
	orch := vega.NewOrchestrator(
		vega.WithLLM(anthropic),
		vega.WithMaxProcesses(50),
		vega.WithContainerManager(cm, tronCfg.WorkingDir),
	)
	defer orch.Shutdown(context.Background())

	// Register custom tools with container support
	customTools := tools.NewPersonaTools(orch, cfg, tronCfg.WorkingDir, tronCfg.TronDir, cm)

	// Create agent
	agent := buildAgent(agentDef, customTools, tronCfg.WorkingDir)

	// Spawn process
	ctx := context.Background()
	proc, err := orch.Spawn(agent,
		vega.WithSupervision(vega.Supervision{
			Strategy:    vega.Restart,
			MaxRestarts: 3,
			Window:      600_000_000_000, // 10 minutes in nanoseconds
		}),
		vega.WithWorkDir(tronCfg.WorkingDir),
	)
	if err != nil {
		log.Fatalf("Failed to spawn %s: %v", selectedAgent, err)
	}

	fmt.Printf("Chat with %s (type 'exit' to quit)\n", selectedAgent)
	fmt.Println("=========================================")

	reader := os.Stdin
	buf := make([]byte, 4096)

	for {
		fmt.Print("\nYou: ")
		n, err := reader.Read(buf)
		if err != nil {
			break
		}

		input := string(buf[:n-1]) // trim newline
		if input == "exit" || input == "quit" {
			break
		}

		if input == "" {
			continue
		}

		// Stream response
		stream, err := proc.SendStream(ctx, input)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}

		fmt.Printf("\n%s: ", selectedAgent)
		for chunk := range stream.Chunks() {
			fmt.Print(chunk)
		}
		if err := stream.Err(); err != nil {
			fmt.Printf("\nError: %v\n", err)
		}
		fmt.Println()
	}

	fmt.Println("\nGoodbye!")
}

// buildAgent creates a vega.Agent from a DSL agent definition
func buildAgent(def *dsl.Agent, customTools *tools.PersonaTools, workingDir string) vega.Agent {
	vegaTools := vega.NewTools(
		vega.WithSandbox(workingDir),
	)
	vegaTools.RegisterBuiltins()

	// Register custom tools
	customTools.RegisterTo(vegaTools)

	// Filter to only requested tools
	if len(def.Tools) > 0 {
		vegaTools = vegaTools.Filter(def.Tools...)
	}

	agent := vega.Agent{
		Name:   def.Name,
		Model:  def.Model,
		System: vega.StaticPrompt(def.System),
		Tools:  vegaTools,
	}

	if def.Temperature != nil {
		agent.Temperature = def.Temperature
	}

	if def.Budget != "" {
		agent.Budget = parseBudget(def.Budget)
	}

	return agent
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

// parseWindow converts a duration string like "10m" to time.Duration
func parseWindow(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}
