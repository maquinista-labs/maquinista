# Volta

Unified agent orchestration platform. Combines Telegram bot management, pull-based task coordination via PostgreSQL, and pluggable agent runners into a single CLI.

## Prerequisites

- Go 1.21+
- Docker and Docker Compose
- tmux
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (or another supported agent runner)

## Quick Start

### 1. Telegram Setup

1. Talk to [@BotFather](https://t.me/BotFather) on Telegram and create a new bot (`/newbot`). Copy the bot token.
2. Create a Telegram group, convert it to a **Supergroup**, and enable **Topics** (Settings > Topics > On).
3. Add your bot to the group and promote it to **admin** (needs permission to send/manage messages).
4. Get your **chat ID** and **user ID**:
   ```bash
   curl -s "https://api.telegram.org/bot<YOUR_TOKEN>/getUpdates" | jq '.result[-1].message'
   ```
   - `chat.id` is the group ID (negative number like `-100xxxxxxxxxx`)
   - `from.id` is your user ID

### 2. Configure Environment

Create a `.env` file in the project root:

```bash
# Required
TELEGRAM_BOT_TOKEN=123456:ABC-DEF...
ALLOWED_USERS=YOUR_USER_ID
ALLOWED_GROUPS=-100XXXXXXXXXX
DATABASE_URL=postgres://volta:volta@localhost:5432/voltadb?sslmode=disable

# Optional
VOLTA_DIR=~/.volta
TMUX_SESSION_NAME=volta
VOLTA_DEFAULT_PROJECT=myproject
# VOLTA_QUEUE_TOPIC_ID=123       # topic ID for queue status notifications
# VOLTA_APPROVALS_TOPIC_ID=456   # topic ID for approval requests
```

### 3. Start Database

```bash
docker compose -f docker/docker-compose.yml up -d
```

This starts PostgreSQL 16 on port 5432 with credentials `volta:volta` and database `voltadb`.

To also start pgAdmin (available at http://localhost:5050):

```bash
docker compose -f docker/docker-compose.yml --profile debug up -d
```

### 4. Build and Migrate

```bash
make build
./volta migrate
```

### 5. Run

```bash
# Start Telegram bot
./volta start

# Start Telegram bot + autonomous orchestrator
./volta start --orchestrate --orchestrate-project myproject

# Stop Volta (kills bot, tmux session, DB agents)
./volta stop

# Standalone orchestrator (no Telegram)
./volta orchestrate --project myproject --max-agents 3
```

## How It Works

Each Telegram **topic** maps to one **tmux window** running a Claude Code session. Send a message in a topic and Volta spawns an agent, forwards your text, and streams responses back.

For automated work, the **orchestrator** polls the task queue, spawns agents for ready tasks, and reconciles dead sessions. Tasks flow through: `draft` -> `ready` -> `claimed` -> `done`.

## Commands

### Telegram Bot

| Command | Description |
|---------|-------------|
| `/menu` | Show command menu |
| `/c_screenshot` | Terminal screenshot |
| `/c_esc` | Send Escape to interrupt agent |
| `/c_clear` | Forward /clear to agent |
| `/p_bind` | Bind a project to this topic |
| `/p_tasks` | List tasks for bound project |
| `/p_add` | Create a new task |
| `/t_pick` | Assign a task to the agent |
| `/t_auto` | Auto-claim and work tasks |
| `/t_batch` | Work a list of tasks in order |
| `/t_pickw` | Pick task in isolated worktree |
| `/t_merge` | Merge a branch |
| `/observe` | Observe an agent from this topic |

### CLI

```
volta start          Start Telegram bot daemon
volta stop           Stop Volta and clean up resources
volta orchestrate    Run autonomous orchestrator loop
volta run            Spawn agents in tmux
volta spawn <name>   Spawn a single named agent
volta status         Table view of all tasks
volta tree           Dependency tree with status symbols
volta add            Create a task
volta show <id>      Print task spec + context
volta spec sync      Sync spec files to database
volta spec validate  Validate spec files
volta migrate        Run database migrations
volta agents         Show running agents
volta logs <agent>   Capture agent's tmux output
volta kill <agent>   Kill agent and release tasks
volta merge          Process merge queue
volta attach         Attach to tmux session
volta search <q>     Full-text search across task context
volta prompt auto    Generate auto-mode prompt
volta prompt single  Generate single-task prompt
```

### Agent Scripts

These are available in agent tmux sessions via `$PATH`:

| Script | Description |
|--------|-------------|
| `volta-claim` | Claim next ready task from queue |
| `volta-done <id> "<summary>"` | Run tests, mark task done |
| `volta-pick <id>` | Claim a specific task by ID |
| `volta-observe <id> "<note>"` | Record an observation |
| `volta-handoff <id> "<note>"` | Record handoff before long operations |

## Task Specs

Define tasks as markdown files with YAML frontmatter:

```markdown
---
id: my-task
title: "Implement feature X"
priority: 8
depends_on:
  - setup-task
test_cmd: "go test ./..."
requires_approval: false
---
Detailed specification goes here.
```

Sync to database:

```bash
volta spec sync --dir .specs/ --project myproject --release
```

## Agent Runners

Volta supports pluggable agent runners via the `--runner` flag:

| Runner | Description |
|--------|-------------|
| `claude` | Claude Code CLI (default) |
| `opencode` | OpenCode CLI |
| `custom` | Arbitrary binary with Go template commands |

```bash
volta run --runner opencode --agents 2
volta orchestrate --project myproject --runner claude
```

## Project Structure

```
cmd/volta/          CLI entry point and all subcommands
internal/
  agent/            Agent lifecycle (spawn, kill, heartbeat)
  bot/              Telegram bot handlers and UI
  bridge/           CLI bridge for task operations
  config/           Configuration loading
  db/               PostgreSQL queries and migrations
  git/              Git operations (worktrees, merge)
  listener/         PostgreSQL NOTIFY event listener
  monitor/          Session output monitor
  orchestrator/     Autonomous task orchestrator
  prompt/           Prompt generation for agents
  queue/            Telegram message queue (flood control)
  render/           Output rendering
  runner/           Pluggable agent runner interface
  spec/             Task spec file parser and sync
  state/            Persistent state management
  tmux/             tmux session/window management
  tui/              Terminal UI components
scripts/            Agent shell scripts (volta-claim, volta-done, etc.)
docker/             Docker Compose for PostgreSQL
```
