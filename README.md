# Code Agent
Code Agent is an experimental AI-native coding agent runtime written in Go.

This project is not just a chatbot wrapper. Its goal is to explore the core loop of a real coding agent:

```plain text
Goal → Model Decision → Tool Execution → Observation → Next Decision → Final
```

The current version is a minimal runnable foundation. It supports:

* Running a CLI command
* Calling DeepSeek through an OpenAI-compatible chat completion API
* Executing a simple agent loop
* Letting the model output structured JSON decisions
* Executing the first workspace tool: list_files
* Returning tool observations back to the model
* Producing a final answer after one or more reasoning/action steps

## Project Status

This project is in the earliest P0 stage.

The current goal is to build a small but correct agent runtime foundation before adding advanced features.

Implemented:

* `codeagent ask "..."`
* `codeagent run "..."`
* DeepSeek provider via OpenAI-compatible API
* In-memory agent state
* JSON-based model decision protocol
* Tool registry
* `list_files` tool
* `read_file` tool
* `grep` tool
* Basic trace output in terminal

Not implemented yet:

* `apply_patch`
* `git_diff`
* `run_command`
* SQLite memory
* Native model tool calls
* Streaming output
* Permission confirmation
* Sandbox policy enforcement
* GUI

## Why This Project Exists

The purpose of this project is to understand and build an AI-native coding agent from first principles.

Many coding assistants can generate a large amount of code quickly, but this project intentionally starts small and transparent. Every part of the runtime should be understandable, observable, and controllable.

The design principle is:
```plain text
The model decides what to do next.
The runtime controls what can actually be executed.
Tools are explicit and observable.
Every step should be traceable.
```

## Architecture

Current high-level architecture:
```plain text
cmd/codeagent
  ↓
internal/app
  ↓
internal/agent
  ↓
internal/model
  ↓
internal/tools
  ↓
workspace
```

Main packages:
```plain text
cmd/codeagent/
  CLI entrypoint.

internal/app/
  Application configuration loading.

internal/agent/
  Core agent runtime: decision, state, loop, step.

internal/model/
  LLM provider abstraction and OpenAI-compatible implementation.

internal/prompt/
  System prompt for JSON decision protocol.

internal/tools/
  Tool interface and registry.

internal/tools/filesystem/
  Filesystem tools, including list_files and read_file.

internal/tools/search/
  Search tools, currently including grep.

internal/workspace/
  Future workspace context management.

internal/sandbox/
  Future permission and execution policy.

internal/memory/
  Future session and trace persistence.

internal/trace/
  Future structured event tracing.

internal/ui/
  Future terminal interaction helpers.

pkg/agentapi/
  Future public API types.
```

Core Agent Loop

The runtime is built around a simple loop:

```plain text
1. User gives a goal
2. Agent sends goal and system prompt to the model
3. Model returns a JSON decision
4. Runtime parses the decision
5. If it is a tool call, runtime executes the tool
6. Tool result becomes an observation
7. Observation is sent back to the model
8. Model returns the next decision
9. Agent stops when it receives final_answer
```

Example model decision:

```json
{
  "type": "tool_call",
  "tool": "list_files",
  "input": {
    "path": "."
  },
  "reason": "I need to inspect the workspace structure first."
}
```

Example final answer:
```json
{
  "type": "final_answer",
  "message": "This project is a Go-based AI-native coding agent runtime..."
}
```

## Requirements

* Go 1.22+
* DeepSeek API key

## Setup

Install dependencies:

```bash
go mod tidy
```

Create local config:

```bash
cp config.example.yaml config.yaml
```

Set your DeepSeek API key:

```bash
export DEEPSEEK_API_KEY="your_api_key"
```

Configuration

Example config.example.yaml:

```yaml
model:
  provider: deepseek
  base_url: "https://api.deepseek.com"
  model: "deepseek-v4-flash"
  temperature: 0.2

agent:
  max_steps: 8

workspace:
  root: "."
```

Usage

Ask a normal question:

```bash
go run ./cmd/codeagent ask "你是谁"
```

Run the agent loop:
```bash
go run ./cmd/codeagent run "解释下这个项目结构"
```

Example output:
```plain text
[1] decision=tool_call tool=list_files reason=To understand the project structure, I need to list the files and directories in the current workspace.

[observation]
cmd/
internal/
go.mod
README.md
...

[2] decision=final_answer

Final:
该项目是一个名为 CodeAgent 的 AI 编码代理...
```

## Current Design Rules

The project currently follows these rules:

```plain text
No database before the basic agent loop works.
No uncontrolled file modification.
No shell execution before permission control is designed.
No hidden automation.
No complex framework before the runtime is understandable.
```

The first milestone is not to build a full Claude Code replacement.

The first milestone is to build a correct, observable, minimal AI-native agent runtime.

## Roadmap

### P0: Read-only Agent Foundation

* [x] CLI entrypoint
* [x] DeepSeek provider
* [x] Agent loop
* [x] JSON decision protocol
* [x] Tool registry
* [x] `list_files`
* [x] `read_file`
* [x] `grep`

### P1: Code Editing

* apply_patch
* git_diff
* User confirmation before applying patches
* Rollback strategy

### P2: Command Execution

* run_command
* Command allowlist
* Timeout control
* Basic sandbox policy
* Test/fix loop

### P3: Memory and Trace

* SQLite session store
* Step persistence
* Tool call persistence
* Trace viewer

### P4: Local and Cloud Runtime Split

* Local tool runtime
* Remote tool runtime abstraction
* Workspace adapter
* Server-side sandbox experiment

## Philosophy

Code Agent is built around one simple belief:

```plain text
An AI-native agent is not a fixed workflow.
It is a runtime where the model can decide the next step,
while the system provides tools, boundaries, state, and observation.
```

## P0 Verification

```bash
go run ./cmd/codeagent ask "你是谁"
go run ./cmd/codeagent run "解释这个项目结构"
go run ./cmd/codeagent run "解释 cmd/codeagent/main.go 是怎么工作的"
go run ./cmd/codeagent run "Provider 接口在哪里定义？它是如何被调用的？"
go run ./cmd/codeagent run "Agent loop 的核心流程是什么？请基于代码解释"
```

The current version is the first read-only heartbeat of the agent runtime.