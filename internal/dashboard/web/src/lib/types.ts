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

export type ConversationItem = {
  /** "inbox" | "outbox" — direction. */
  kind: "inbox" | "outbox";
  id: string;
  agent_id: string;
  from_kind?: "user" | "agent" | "system";
  excerpt: string | null;
  at: string;
};
