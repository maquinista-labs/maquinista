# Maquinista

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
DATABASE_URL=postgres://maquinista:maquinista@localhost:5434/maquinistadb?sslmode=disable

# Optional
MAQUINISTA_DIR=~/.maquinista
TMUX_SESSION_NAME=maquinista
MAQUINISTA_DEFAULT_PROJECT=myproject
# Auto-spawned default agent. `maquinista start` opens a tmux window
# running $CLAUDE_COMMAND with AGENT_ID exported, unless --no-agent is
# passed or an agent with the same id is already registered.
MAQUINISTA_DEFAULT_AGENT=maquinista
# Working directory for the default agent (defaults to $HOME).
# MAQUINISTA_DEFAULT_CWD=~/code/maquinista
# MAQUINISTA_QUEUE_TOPIC_ID=123       # topic ID for queue status notifications
# MAQUINISTA_APPROVALS_TOPIC_ID=456   # topic ID for approval requests
```

### 3. Start Database

```bash
docker compose -f docker/docker-compose.yml up -d
```

This starts PostgreSQL 16 on host port **5434** (container 5432) with credentials `maquinista:maquinista` and database `maquinistadb`.

To also start pgAdmin (available at http://localhost:5051):

```bash
docker compose -f docker/docker-compose.yml --profile debug up -d
```

### 4. Build and Migrate

```bash
make build
./maquinista migrate
```

### 5. Run

```bash
# Start Telegram bot (auto-spawns default agent named "maquinista")
./maquinista start

# Override the default agent id or working dir
./maquinista start --agent otavio --agent-cwd ~/code/myproject

# Skip the auto-spawn entirely (agents must be started manually)
./maquinista start --no-agent

# Start Telegram bot + autonomous orchestrator
./maquinista start --orchestrate --orchestrate-project myproject

# Stop Maquinista (kills bot, tmux session, DB agents)
./maquinista stop

# Standalone orchestrator (no Telegram)
./maquinista orchestrate --project myproject --max-agents 3
```

## How It Works

Each Telegram **topic** maps to one **tmux window** running a Claude Code session. Send a message in a topic and Maquinista spawns an agent, forwards your text, and streams responses back.

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
maquinista start          Start Telegram bot daemon
maquinista stop           Stop Maquinista and clean up resources
maquinista orchestrate    Run autonomous orchestrator loop
maquinista run            Spawn agents in tmux
maquinista spawn <name>   Spawn a single named agent
maquinista status         Table view of all tasks
maquinista tree           Dependency tree with status symbols
maquinista add            Create a task
maquinista show <id>      Print task spec + context
maquinista spec sync      Sync spec files to database
maquinista spec validate  Validate spec files
maquinista migrate        Run database migrations
maquinista agents         Show running agents
maquinista logs <agent>   Capture agent's tmux output
maquinista kill <agent>   Kill agent and release tasks
maquinista merge          Process merge queue
maquinista attach         Attach to tmux session
maquinista search <q>     Full-text search across task context
maquinista prompt auto    Generate auto-mode prompt
maquinista prompt single  Generate single-task prompt
```

### Agent Scripts

These are available in agent tmux sessions via `$PATH`:

| Script | Description |
|--------|-------------|
| `maquinista-claim` | Claim next ready task from queue |
| `maquinista-done <id> "<summary>"` | Run tests, mark task done |
| `maquinista-pick <id>` | Claim a specific task by ID |
| `maquinista-observe <id> "<note>"` | Record an observation |
| `maquinista-handoff <id> "<note>"` | Record handoff before long operations |

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
maquinista spec sync --dir .specs/ --project myproject --release
```

## Agent Runners

Maquinista supports pluggable agent runners via the `--runner` flag:

| Runner | Description |
|--------|-------------|
| `claude` | Claude Code CLI (default) |
| `opencode` | OpenCode CLI |
| `custom` | Arbitrary binary with Go template commands |

```bash
maquinista run --runner opencode --agents 2
maquinista orchestrate --project myproject --runner claude
```

## Project Structure

```
cmd/maquinista/          CLI entry point and all subcommands
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
scripts/            Agent shell scripts (maquinista-claim, maquinista-done, etc.)
docker/             Docker Compose for PostgreSQL
```
