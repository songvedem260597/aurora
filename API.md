# OpenAI-Compatible API Documentation

## Base URL

```text
https://api--chatgpt-api--x846ccphx9g8.code.run
```

## Authentication

All protected endpoints use Bearer authentication:

```http
Authorization: Bearer YOUR_API_KEY
```

Two key types are supported:

- **Master API key**: server/operator credential. Keep it private. It can issue temporary API keys.
- **Temporary API key**: issued by `POST /v1/api-keys`. It is valid for exactly **24 hours** and can call the normal API endpoints, but it cannot issue additional keys.

### Issue a 24-hour API key

`POST /v1/api-keys`

This endpoint requires the master API key.

```bash
curl -X POST \
  https://api--chatgpt-api--x846ccphx9g8.code.run/v1/api-keys \
  -H "Authorization: Bearer MASTER_API_KEY"
```

Example response:

```json
{
  "object": "api_key",
  "key": "sk-gw-...",
  "created_at": 1787110000,
  "expires_at": 1787196400,
  "expires_in": 86400
}
```

Use the returned key like this:

```http
Authorization: Bearer sk-gw-...
```

After 24 hours, the key returns `401` and a new key must be issued.

## Models

### GET /v1/models

Returns the available model list and capability metadata.

```bash
curl https://api--chatgpt-api--x846ccphx9g8.code.run/v1/models \
  -H "Authorization: Bearer YOUR_API_KEY"
```

Example response shape:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5-6",
      "object": "model",
      "owned_by": "openai",
      "capabilities": {
        "vision": true,
        "pdf": true,
        "tools": true,
        "reasoning": true
      }
    }
  ]
}
```

### GET /v1/models/info?id=MODEL_ID

Returns detailed metadata for one model.

```bash
curl "https://api--chatgpt-api--x846ccphx9g8.code.run/v1/models/info?id=gpt-5-6" \
  -H "Authorization: Bearer YOUR_API_KEY"
```

## Chat Completions

### POST /v1/chat/completions

OpenAI-compatible chat completion endpoint.

```bash
curl https://api--chatgpt-api--x846ccphx9g8.code.run/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5-6",
    "messages": [
      {
        "role": "user",
        "content": "Hello"
      }
    ]
  }'
```

Example request body:

```json
{
  "model": "gpt-5-6",
  "messages": [
    {
      "role": "user",
      "content": "Hello"
    }
  ]
}
```

## Streaming

Set `stream` to `true` to receive Server-Sent Events (SSE):

```json
{
  "model": "gpt-5-6",
  "stream": true,
  "messages": [
    {
      "role": "user",
      "content": "Hello"
    }
  ]
}
```

Typical stream format:

```text
data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":"Hello"}}]}

data: [DONE]
```

## Tool Calling

The API supports OpenAI-compatible function/tool calling.

Example request:

```json
{
  "model": "gpt-5-6-thinking",
  "messages": [
    {
      "role": "user",
      "content": "Check the current working directory"
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "bash",
        "description": "Execute a shell command",
        "parameters": {
          "type": "object",
          "properties": {
            "command": {
              "type": "string"
            }
          },
          "required": ["command"]
        }
      }
    }
  ]
}
```

When a tool is requested, the response uses standard `tool_calls` and `finish_reason: "tool_calls"`. The client executes the tool and sends the result back in a subsequent message with `role: "tool"`.

## Responses API

### POST /v1/responses

A Responses-compatible endpoint is available for clients that use the newer response format.

## Files

### POST /v1/files

Uploads a file for supported API workflows.

## Images

Available image endpoints:

```text
POST /v1/images/generations
POST /v1/images/edits
POST /v1/images/variations
```

## Audio

Available audio endpoints:

```text
POST /v1/audio/speech
POST /v1/audio/transcriptions
POST /v1/audio/translations
```

## OpenCode Configuration

Example OpenAI-compatible provider configuration:

```json
{
  "provider": {
    "gateway": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "https://api--chatgpt-api--x846ccphx9g8.code.run/v1",
        "apiKey": "YOUR_API_KEY"
      }
    }
  }
}
```

Example model reference:

```text
gateway/gpt-5-6-thinking
```

## Errors

Errors use a JSON error object:

```json
{
  "error": {
    "message": "Invalid or missing API key",
    "type": "authentication_error",
    "code": "invalid_api_key"
  }
}
```

Common HTTP status codes:

| Code | Meaning |
|---|---|
| 400 | Invalid request |
| 401 | Invalid or expired API key |
| 403 | Request not permitted |
| 404 | Resource not found |
| 429 | Rate limit |
| 500 | Server error |
| 503 | Service or API-key administration unavailable |

## Health Check

Use the protected models endpoint to verify authentication and API availability:

```bash
curl https://api--chatgpt-api--x846ccphx9g8.code.run/v1/models \
  -H "Authorization: Bearer YOUR_API_KEY"
```
