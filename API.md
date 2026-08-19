# Aurora OpenAI-Compatible API Documentation

## Base URL

```
https://api--chatgpt-api--x846ccphx9g8.code.run
```

## Authentication

All requests require Bearer authentication:

```http
Authorization: Bearer YOUR_ACCESS_TOKEN
```

## Models

### GET /v1/models

Returns available models.

Example:

```bash
curl https://api--chatgpt-api--x846ccphx9g8.code.run/v1/models \
  -H "Authorization: Bearer YOUR_ACCESS_TOKEN"
```

## Chat Completions

### POST /v1/chat/completions

OpenAI compatible chat completion endpoint.

Example request:

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

Enable Server Sent Events:

```json
{
  "stream": true
}
```

Response format:

```
data: {"choices":[{"delta":{"content":"Hello"}}]}

data: [DONE]
```

## Tool Calling

Aurora supports OpenAI compatible tool calls.

Supported agent flow:

```
User Request
    |
    v
OpenCode
    |
    v
Aurora API
    |
    v
Tool Call
    |
    +-- bash
    +-- read
    +-- write
    +-- edit
    |
    v
Tool Result
    |
    v
Final Response
```

Example tool request:

```json
{
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "bash",
        "description": "Execute shell command"
      }
    }
  ]
}
```

## OpenCode Configuration

```json
{
  "provider": {
    "aurora": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "https://api--chatgpt-api--x846ccphx9g8.code.run/v1",
        "apiKey": "YOUR_ACCESS_TOKEN"
      }
    }
  }
}
```

## Errors

```json
{
  "error": {
    "message": "Error message",
    "type": "api_error"
  }
}
```

Common codes:

| Code | Meaning |
|---|---|
|401|Invalid token|
|400|Invalid request|
|429|Rate limit|
|500|Server error|

## Health Check

Use `/v1/models` to verify the API is online.
