// Shared DTOs between Route Handlers and Server / Client Components.
// Kept deliberately small; the Route Handler does the SQL, the
// component does the render, the types pin the wire format.

export type AgentStatus = "idle" | "working" | "dead" | "archived";

export type AgentListItem = {
  id: string;
  handle: string | null;
  runner: string;
  model: string | null;
  role: string;
  status: AgentStatus;
  stop_requested: boolean;
  tmux_window: string | null;
  /** ISO 8601 timestamp from agents.started_at. */
  started_at: string;
  /** ISO 8601 timestamp from agents.last_seen, or null. */
  last_seen: string | null;
  /** Truncated text extracted from agent_outbox.content, or null. */
  last_outbox_excerpt: string | null;
  /** ISO 8601 timestamp of last outbox row, or null. */
  last_outbox_at: string | null;
  /** Count of agent_inbox rows with status IN ('pending','processing'). */
  unread_inbox_count: number;
  /** persona slug from agent_settings, e.g. "planner" or "main". */
  persona: string | null;
};

export type InboxRow = {
  id: string;
  agent_id: string;
  from_kind: "user" | "agent" | "system";
  from_id: string | null;
  status:
    | "pending"
    | "processing"
    | "processed"
    | "failed"
    | "dead";
  origin_channel: string | null;
  origin_user_id: string | null;
  excerpt: string | null;
  enqueued_at: string;
};

export type OutboxRow = {
  id: string;
  agent_id: string;
  in_reply_to: string | null;
  status: "pending" | "routing" | "routed" | "failed";
  excerpt: string | null;
  created_at: string;
};

// GlobalInboxRow: an InboxRow enriched with the owning agent's
// handle (if set) — the /inbox feed shows rows from across every
// agent, so the per-row agent label would otherwise be ambiguous.
export type GlobalInboxRow = InboxRow & {
  agent_handle: string | null;
};

// ConversationRow: one row per conversation_id for the global /chats
// feed. Merges inbox + outbox into a single timeline metadata shape
// and surfaces pending_count so operators can spot unresolved items
// at a glance.
export type ConversationRow = {
  // conversation_id is the real UUID when the thread is a multi-agent
  // a2a conversation; null for single-agent Telegram-topic chats.
  // UI uses thread_key as the React key and decides whether to add
  // a `conversation=` filter to the agent detail link based on
  // whether conversation_id is set.
  conversation_id: string | null;
  thread_key: string;
  agent_id: string;
  agent_handle: string | null;
  last_at: string;
  preview: string | null;
  msg_count: number;
  pending_count: number;
};

export type ConversationItem = {
  /** "inbox" | "outbox" — direction. */
  kind: "inbox" | "outbox";
  id: string;
  agent_id: string;
  from_kind?: "user" | "agent" | "system";
  excerpt: string | null;
  at: string;
};

export type KPIs = {
  active_agents: number;
  total_agents: number;
  inbox_in_flight: number;
  outbox_pending: number;
  tokens_today: { input: number; output: number };
  cost_today_cents: number;
  cost_month_projected_cents: number;
  cost_by_model: Array<{ model: string; cents: number }>;
};

export type ScheduledJob = {
  id: string;
  name: string;
  kind: "scheduled";
  cron_expr: string;
  timezone: string;
  agent_id: string;
  enabled: boolean;
  next_run_at: string;
  last_run_at: string | null;
};

export type WebhookHandler = {
  id: string;
  name: string;
  kind: "webhook";
  path: string;
  agent_id: string;
  enabled: boolean;
  rate_limit_per_min: number;
};

export type JobsList = {
  scheduled: ScheduledJob[];
  webhooks: WebhookHandler[];
};

export type SystemHealth = {
  pg: {
    total: number;
    idle: number;
    waiting: number;
  };
  uptime_ms: number;
  pid: number;
  node_version: string;
  platform: string;
};
