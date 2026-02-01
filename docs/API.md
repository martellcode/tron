# Tron API Reference

This document provides comprehensive documentation for all HTTP API endpoints exposed by the Tron server.

## Base URL

```
http://localhost:3000
```

For production:
```
https://api.hellotron.com
```

## Authentication

Currently, the API does not require authentication. In production, endpoints are protected at the network level.

---

## Health & Status

### GET /health

Simple health check endpoint.

**Response**

```json
{
  "status": "ok"
}
```

**Status Codes**

| Code | Description |
|------|-------------|
| 200 | Server is healthy |

**Example**

```bash
curl http://localhost:3000/health
```

---

## Control Panel API

These endpoints provide visibility into the running system for building monitoring dashboards and control panels.

### GET /api/status

Returns an overview of the system status including process counts, session counts, and available personas.

**Response**

```json
{
  "status": "ok",
  "uptime": "2h30m15s",
  "process_count": 5,
  "session_count": 2,
  "personas": ["Tony", "Maya", "Alex", "Jordan", "Riley"],
  "active_personas": ["Tony", "Maya"]
}
```

**Response Fields**

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | Server status, always "ok" if responding |
| `uptime` | string | Server uptime duration (optional) |
| `process_count` | integer | Total number of active processes |
| `session_count` | integer | Number of active caller sessions |
| `personas` | array | List of all configured persona names |
| `active_personas` | array | List of personas with running life loops (optional) |

**Example**

```bash
curl http://localhost:3000/api/status
```

---

### GET /api/processes

Returns detailed information about all running processes in the orchestrator.

**Response**

```json
[
  {
    "id": "a1b2c3d4",
    "agent": "Tony",
    "name": "tony-main",
    "status": "running",
    "task": "Handle incoming voice call",
    "started_at": "2024-01-15T10:30:00Z",
    "metrics": {
      "input_tokens": 15420,
      "output_tokens": 8230,
      "total_tokens": 23650,
      "llm_calls": 12,
      "tool_calls": 8,
      "estimated_cost": 0.0945,
      "duration_ms": 125000
    }
  },
  {
    "id": "e5f6g7h8",
    "agent": "Maya",
    "name": "",
    "status": "running",
    "task": "",
    "started_at": "2024-01-15T11:45:00Z",
    "metrics": {
      "input_tokens": 3200,
      "output_tokens": 1800,
      "total_tokens": 5000,
      "llm_calls": 3,
      "tool_calls": 1,
      "estimated_cost": 0.02,
      "duration_ms": 45000
    }
  }
]
```

**Response Fields (per process)**

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Unique process identifier (8 character UUID prefix) |
| `agent` | string | Name of the agent running this process |
| `name` | string | Registered name for the process (empty if not registered) |
| `status` | string | Process status: `pending`, `running`, `completed`, `failed` |
| `task` | string | Task description (if set) |
| `started_at` | string | ISO 8601 timestamp when process started |
| `metrics` | object | Process metrics (see below) |

**Metrics Fields**

| Field | Type | Description |
|-------|------|-------------|
| `input_tokens` | integer | Total input tokens consumed |
| `output_tokens` | integer | Total output tokens generated |
| `total_tokens` | integer | Sum of input and output tokens |
| `llm_calls` | integer | Number of LLM API calls (iterations) |
| `tool_calls` | integer | Number of tool invocations |
| `estimated_cost` | float | Estimated cost in USD |
| `duration_ms` | integer | Time since process started in milliseconds |

**Process Status Values**

| Status | Description |
|--------|-------------|
| `pending` | Process created but not yet running |
| `running` | Process is actively running |
| `completed` | Process finished successfully |
| `failed` | Process encountered an error |

**Example**

```bash
curl http://localhost:3000/api/processes
```

**Example: Filter running processes (client-side)**

```bash
curl http://localhost:3000/api/processes | jq '[.[] | select(.status == "running")]'
```

---

### GET /api/sessions

Returns information about all active caller sessions. Sessions map callers (identified by phone number or request ID) to their associated Tony process.

**Response**

```json
[
  {
    "caller_id": "+14155551234",
    "process_id": "a1b2c3d4",
    "agent": "Tony",
    "status": "running"
  },
  {
    "caller_id": "anonymous-1705312200000000000",
    "process_id": "i9j0k1l2",
    "agent": "Tony",
    "status": "running"
  }
]
```

**Response Fields (per session)**

| Field | Type | Description |
|-------|------|-------------|
| `caller_id` | string | Caller identifier (phone number, request ID, or anonymous ID) |
| `process_id` | string | ID of the process handling this caller |
| `agent` | string | Agent name (typically "Tony") |
| `status` | string | Process status: `pending`, `running`, `completed`, `failed` |

**Caller ID Formats**

| Format | Description |
|--------|-------------|
| `+1XXXXXXXXXX` | Phone number (E.164 format) |
| `req-XXXXX` | Request ID from X-Request-ID header |
| `anonymous-XXXXXXXXX` | Auto-generated ID for anonymous callers |

**Example**

```bash
curl http://localhost:3000/api/sessions
```

---

## Chat Completion API

OpenAI-compatible chat completion endpoint used by VAPI and other integrations.

### POST /chat/completions

Also available at: `POST /v1/chat/completions`

Process a chat completion request. Supports both streaming and non-streaming responses.

**Request Headers**

| Header | Description |
|--------|-------------|
| `Content-Type` | `application/json` |
| `X-Vapi-Caller-Phone` | (Optional) Caller's phone number |
| `X-Vapi-Call-ID` | (Optional) VAPI call identifier |
| `X-Request-ID` | (Optional) Request identifier for session tracking |

**Request Body**

```json
{
  "model": "tony",
  "messages": [
    {
      "role": "system",
      "content": "You are Tony, the CTO."
    },
    {
      "role": "user",
      "content": "What's on your mind today?"
    }
  ],
  "stream": true,
  "temperature": 0.7,
  "max_tokens": 1000
}
```

**Request Fields**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | No | Model name (ignored, uses configured agent) |
| `messages` | array | Yes | Array of chat messages |
| `stream` | boolean | No | Enable streaming response (default: false) |
| `temperature` | float | No | Sampling temperature (0.0-1.0) |
| `max_tokens` | integer | No | Maximum tokens in response |

**Message Object**

| Field | Type | Description |
|-------|------|-------------|
| `role` | string | Message role: `system`, `user`, or `assistant` |
| `content` | string | Message content |

**Non-Streaming Response**

```json
{
  "id": "chatcmpl-1705312200000000000",
  "object": "chat.completion",
  "created": 1705312200,
  "model": "tony",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "I've been thinking about our architecture..."
      },
      "finish_reason": "stop"
    }
  ]
}
```

**Streaming Response**

When `stream: true`, responses are sent as Server-Sent Events (SSE):

```
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1705312200,"model":"tony","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1705312200,"model":"tony","choices":[{"index":0,"delta":{"content":"I've "}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1705312200,"model":"tony","choices":[{"index":0,"delta":{"content":"been "}}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1705312200,"model":"tony","choices":[{"index":0,"delta":{},"finish_reason":"stop"}}]}

data: [DONE]
```

**Example: Non-streaming**

```bash
curl -X POST http://localhost:3000/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Hello Tony!"}],
    "stream": false
  }'
```

**Example: Streaming**

```bash
curl -X POST http://localhost:3000/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Hello Tony!"}],
    "stream": true
  }'
```

---

## VAPI Integration

### POST /vapi/events

Webhook endpoint for VAPI voice events. Receives call lifecycle events and processes them accordingly.

**Request Body**

VAPI sends various event types. See [VAPI documentation](https://docs.vapi.ai) for full event specifications.

**Common Event Types**

| Event | Description |
|-------|-------------|
| `call.started` | A new call has begun |
| `call.ended` | A call has ended |
| `speech.recognized` | Speech has been transcribed |
| `assistant.message` | Assistant response is ready |

**Example Event**

```json
{
  "type": "call.started",
  "call": {
    "id": "call-123",
    "from": "+14155551234",
    "to": "+14155555678"
  }
}
```

---

## ElevenLabs Integration

### POST /v1/elevenlabs-llm

LLM endpoint for ElevenLabs Conversational AI integration.

**Request Body**

```json
{
  "messages": [
    {
      "role": "user",
      "content": "Hello!"
    }
  ],
  "stream": true
}
```

### WS /ws/elevenlabs

WebSocket endpoint for real-time voice conversations with ElevenLabs.

**Connection**

```javascript
const ws = new WebSocket('ws://localhost:3000/ws/elevenlabs');
```

**Message Format**

Messages are exchanged as JSON objects. See ElevenLabs Conversational AI documentation for protocol details.

---

## Slack Integration

### POST /slack/events

Webhook endpoint for Slack Events API. Handles bot mentions, direct messages, and other Slack events.

**Verification**

Slack sends a verification challenge on initial setup:

```json
{
  "type": "url_verification",
  "challenge": "challenge-string"
}
```

Response:
```json
{
  "challenge": "challenge-string"
}
```

**Event Callback**

```json
{
  "type": "event_callback",
  "event": {
    "type": "app_mention",
    "user": "U1234567890",
    "text": "<@U0987654321> What do you think about this?",
    "channel": "C1234567890",
    "ts": "1705312200.000000"
  }
}
```

---

## Internal Endpoints

These endpoints are for internal use and testing.

### POST /internal/life/trigger

Manually trigger a life loop activity for testing or demos.

**Query Parameters**

| Parameter | Required | Description |
|-----------|----------|-------------|
| `activity` | Yes | Activity to trigger: `news`, `goals`, `team_check`, `reflection`, `journal`, `post` |
| `persona` | No | Specific persona (default: all personas) |

**Response (without activity parameter)**

```json
{
  "error": "Missing 'activity' query parameter",
  "activities": ["news", "goals", "team_check", "reflection", "journal", "post"],
  "personas": ["Tony", "Maya", "Alex", "Jordan", "Riley"],
  "examples": [
    "/internal/life/trigger?activity=post&persona=Tony",
    "/internal/life/trigger?activity=post (triggers all personas)"
  ]
}
```

**Response (single persona)**

```json
{
  "persona": "Tony",
  "activity": "post",
  "result": "Posted: Thinking about microservices architecture today..."
}
```

**Response (all personas)**

```json
{
  "activity": "post",
  "results": {
    "Tony": "Posted: Working on the new API design...",
    "Maya": "Posted: Analyzing our Q1 marketing metrics...",
    "Alex": "Posted: Reflecting on our company vision...",
    "Jordan": "Posted: Reviewing the budget forecasts...",
    "Riley": "Posted: User research insights from today..."
  }
}
```

**Example**

```bash
# Trigger post activity for Tony
curl "http://localhost:3000/internal/life/trigger?activity=post&persona=Tony"

# Trigger news activity for all personas
curl "http://localhost:3000/internal/life/trigger?activity=news"
```

### GET /internal/caddy-ask

On-demand TLS verification endpoint for Caddy. Used to validate subdomain requests.

---

## Error Handling

All endpoints return standard HTTP status codes:

| Code | Description |
|------|-------------|
| 200 | Success |
| 400 | Bad Request - Invalid input |
| 404 | Not Found |
| 405 | Method Not Allowed |
| 500 | Internal Server Error |
| 503 | Service Unavailable |

**Error Response Format**

```json
{
  "error": {
    "message": "Description of the error",
    "type": "error_type"
  }
}
```

---

## CORS

The Control Panel API endpoints (`/api/*`) include CORS headers to allow browser-based access:

```
Access-Control-Allow-Origin: *
```

---

## Rate Limiting

Currently, no rate limiting is enforced at the API level. Rate limiting may be configured at the infrastructure level (Caddy, AWS).

---

## WebSocket Connections

For WebSocket endpoints (`/ws/elevenlabs`), the server supports:

- Upgrade from HTTP/1.1
- Ping/pong keepalive
- Graceful connection closure

---

## SDK Examples

### JavaScript/TypeScript

```typescript
// Fetch system status
const status = await fetch('http://localhost:3000/api/status').then(r => r.json());
console.log(`Running ${status.process_count} processes`);

// List processes
const processes = await fetch('http://localhost:3000/api/processes').then(r => r.json());
const running = processes.filter(p => p.status === 'running');

// Chat completion (streaming)
const response = await fetch('http://localhost:3000/chat/completions', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({
    messages: [{ role: 'user', content: 'Hello!' }],
    stream: true
  })
});

const reader = response.body.getReader();
const decoder = new TextDecoder();

while (true) {
  const { done, value } = await reader.read();
  if (done) break;

  const chunk = decoder.decode(value);
  const lines = chunk.split('\n').filter(line => line.startsWith('data: '));

  for (const line of lines) {
    const data = line.slice(6);
    if (data === '[DONE]') break;

    const parsed = JSON.parse(data);
    const content = parsed.choices[0]?.delta?.content;
    if (content) process.stdout.write(content);
  }
}
```

### Python

```python
import requests

# Fetch system status
status = requests.get('http://localhost:3000/api/status').json()
print(f"Running {status['process_count']} processes")

# List processes
processes = requests.get('http://localhost:3000/api/processes').json()
running = [p for p in processes if p['status'] == 'running']

# Chat completion (non-streaming)
response = requests.post('http://localhost:3000/chat/completions', json={
    'messages': [{'role': 'user', 'content': 'Hello!'}],
    'stream': False
})
print(response.json()['choices'][0]['message']['content'])
```

### curl

```bash
# Health check
curl http://localhost:3000/health

# System status
curl http://localhost:3000/api/status | jq

# List running processes
curl http://localhost:3000/api/processes | jq '[.[] | select(.status == "running")]'

# Chat with Tony
curl -X POST http://localhost:3000/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Hello Tony!"}]}' | jq
```

---

## Changelog

### v1.0.0 (2024-01)

- Initial API documentation
- Added Control Panel API endpoints:
  - `GET /api/status`
  - `GET /api/processes`
  - `GET /api/sessions`
