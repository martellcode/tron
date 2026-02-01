# Tron - Tony's AI Team

Tron is an AI-powered agent orchestration system that creates a virtual executive team consisting of five C-suite personas (Tony, Maya, Alex, Jordan, and Riley) plus specialist team members. Built in Go, it runs as an HTTP server providing voice, chat, and webhook integrations.

## Prerequisites

- Go 1.25+
- Docker (optional, for container-based projects)
- An Anthropic API key

## Building

### Local Development (macOS ARM)

```bash
cd tronvega
go build -o tron ./cmd/tron
```

### Production (Linux AMD64)

For AWS EC2 or other Linux deployments:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tron-linux-amd64 ./cmd/tron
```

## Configuration

### 1. Create the config directory

```bash
mkdir -p ~/.tron
```

### 2. Copy and edit the example environment file

```bash
cp .env.example ~/.tron/.env
```

Edit `~/.tron/.env` with your credentials:

```bash
ANTHROPIC_API_KEY=sk-ant-...        # Required
VAPI_API_KEY=...                     # For voice integration
ELEVENLABS_API_KEY=...               # For voice synthesis
SLACK_BOT_TOKEN=...                  # For Slack integration
```

### 3. Copy the Vega configuration

```bash
cp tron.vega.yaml ~/.tron/tron.vega.yaml
```

The Vega configuration defines all 13 agents with their personalities, tools, and permissions.

## Usage

### Start the HTTP Server

```bash
tron serve [options]
```

Options:
- `-port PORT` - Port to listen on (default: 3000 or `TRON_PORT` env var)
- `-config PATH` - Path to vega config (default: `~/.tron/tron.vega.yaml`)

Example:
```bash
tron serve -port 8080
```

### Interactive CLI Chat

Chat directly with a persona from the command line:

```bash
tron chat [options]
```

Options:
- `-agent NAME` - Agent to chat with: Tony, Maya, Alex, Jordan, or Riley (default: first agent in config)
- `-config PATH` - Path to vega config (default: `~/.tron/tron.vega.yaml`)

Example:
```bash
tron chat -agent Tony
tron chat -agent Maya
```

### Help

```bash
tron help
```

## API Endpoints

When running `tron serve`, the following endpoints are available:

### Core Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Health check |
| `POST /chat/completions` | OpenAI-compatible chat completion API |
| `POST /vapi/events` | VAPI webhook integration for voice calls |
| `POST /v1/elevenlabs-llm` | ElevenLabs voice AI integration |
| `WS /ws/elevenlabs` | WebSocket for voice conversations |
| `POST /slack/events` | Slack bot event handling |

### Control Panel API

| Endpoint | Description |
|----------|-------------|
| `GET /api/status` | System status overview (process count, session count, personas) |
| `GET /api/processes` | List all running processes with metrics |
| `GET /api/sessions` | List all active caller sessions |

#### Example: Get System Status

```bash
curl http://localhost:3000/api/status
```

```json
{
  "status": "ok",
  "process_count": 3,
  "session_count": 1,
  "personas": ["Tony", "Maya", "Alex", "Jordan", "Riley"]
}
```

#### Example: List Processes

```bash
curl http://localhost:3000/api/processes
```

```json
[
  {
    "id": "a1b2c3d4",
    "agent": "Tony",
    "status": "running",
    "started_at": "2024-01-15T10:30:00Z",
    "metrics": {
      "input_tokens": 1500,
      "output_tokens": 800,
      "total_tokens": 2300,
      "llm_calls": 5,
      "tool_calls": 3,
      "estimated_cost": 0.045,
      "duration_ms": 12500
    }
  }
]
```

## The Team

### C-Suite Personas

| Name | Role | Specialty |
|------|------|-----------|
| **Tony** | CTO | Technical architecture, engineering, system design |
| **Maya** | CMO | Marketing, growth, brand strategy, communications |
| **Alex** | CEO | Company vision, leadership, strategic direction |
| **Jordan** | CFO | Finance, budgets, fundraising, financial planning |
| **Riley** | CPO | Product management, UX, user research |

### Specialist Team Members

Engineers, PMs, QA, Security, Data, Frontend, and Mobile specialists are also available and can be spawned by the C-suite personas for delegation.

## Architecture

```
┌─────────────────────────────────────────┐
│           Tron Server (Go)              │
├─────────────────────────────────────────┤
│  HTTP API Server                        │
│  - Chat completions, VAPI, ElevenLabs   │
│  - Slack integration, Webhooks          │
├─────────────────────────────────────────┤
│  Vega Orchestrator                      │
│  - Agent spawning & management          │
│  - Tool registration                    │
│  - Process & container management       │
├─────────────────────────────────────────┤
│  Personas (Tony, Maya, Alex, Jordan,    │
│  Riley + 8 specialists)                 │
├─────────────────────────────────────────┤
│  Life Loop Manager                      │
│  - Autonomous daily routines            │
│  - News, goals, journaling, posting     │
└─────────────────────────────────────────┘
```

## Deployment

### Automated Deployment

```bash
./scripts/deploy.sh [instance-name]
```

### Manual Deployment Steps

1. Build the Linux binary:
   ```bash
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tron-linux-amd64 ./cmd/tron
   ```

2. Upload binary and config:
   ```bash
   ./scripts/upload-binary.sh
   ./scripts/upload-config.sh
   ```

3. SSH to the instance and restart the service:
   ```bash
   ./scripts/ssm.sh prod
   sudo systemctl restart tron
   ```

See `docs/INFRASTRUCTURE.md` for complete production infrastructure documentation.

## Development

### Running Tests

```bash
go test ./...
```

### Integration Tests

```bash
go test -v ./orchestration_test.go
```

### Stress Tests

```bash
go test -v ./stress_test.go
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `ANTHROPIC_API_KEY` | Yes | Claude API key |
| `TRON_PORT` | No | Server port (default: 3000) |
| `WORKING_DIR` | No | Working directory for file operations |
| `AGENTS_DIR` | No | Directory for agent data |
| `VAPI_API_KEY` | No | VAPI voice integration |
| `ELEVENLABS_API_KEY` | No | ElevenLabs voice synthesis |
| `SLACK_BOT_TOKEN` | No | Slack bot integration |
| `SMTP_HOST` | No | Email notifications |

## License

Proprietary - All rights reserved.
