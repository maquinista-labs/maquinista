# Dashboard Inbox Improvements Plan

**Status:** Draft  
**Created:** 2026-04-18

## Overview

This document ranks proposed improvements to the maquinista dashboard's inbox and multi-agent interaction features. Each feature is evaluated against parallels from **openclaw** and **hermes-agent** dashboards, with pros/cons and implementation complexity.

---

## Feature Ranking (Priority Order)

### P0 — Already Working

| Feature | Status | Notes |
|---------|--------|-------|
| `/inbox` page | ✅ shipped | Cross-agent flat feed of inbox activity |
| `/agents` page with unread badges | ✅ shipped | Per-agent unread count on cards |
| `/conversations` page | ✅ shipped | Thread-based inbox + outbox merge |

---

### P1 — High Impact, Moderate Complexity

#### 1. Live Inbox Updates (SSE/Polling)

**Source:** This conversation - user's question about notification flow  
**Parallel:**
- **openclaw:** Real-time session updates via WebSocket (`sessions_subscribe`)
- **hermes:** Stream-based updates via `ServerSentEvents` in web UI
- **linear.app:** Live task updates in activity feed
- **Slack:** Real-time message updates without refresh

**Implementation:**
- Reuse existing `/api/stream` endpoint (see `dashboard-cost-sse.md`)
- Add inbox channel to `pg_notify` triggers
- Frontend: `useSSE` hook already exists for KPIs

**Pros:**
- Essential for multi-agent workflow - see new messages instantly
- Leverages existing SSE infrastructure
- No page refresh needed

**Cons:**
- Adds server load (connection per client)
- Requires database trigger on `agent_inbox` status changes

**Complexity:** Medium (backend triggers + frontend hook)

---

#### 2. Global Navigation Badge (Unread Count)

**Source:** This conversation  
**Parallel:**
- **Gmail:** Unread count in tab title + nav badge
- **Slack:** Red badge on nav items with unread
- **Linear:** Activity indicator on sidebar items
- **openclaw:** "pending" count per session in sidebar

**Implementation:**
- Add `GET /api/inbox/count` endpoint returning total pending
- Add badge to `bottom-nav.tsx` Inbox link
- Update document title with count (like Gmail)

**Pros:**
- Always visible: know if there's work without visiting page
- Low implementation cost
- Matches user expectations from email/chat apps

**Cons:**
- Requires polling or SSE for real-time (same as #1)
- Small UI change but valuable

**Complexity:** Low

---

### P2 — Medium Impact, Lower Complexity

#### 3. Quick Compose from Inbox List

**Source:** This conversation  
**Parallel:**
- **Slack:** Quick reply inline in channel list
- **Linear:** Inline comment input on task cards
- **notion:** Quick capture in sidebar

**Implementation:**
- Add expand button on each inbox row
- Inline textarea + send button
- POST to `/api/agents/[id]/inbox` (already exists)

**Pros:**
- Don't navigate to agent page for simple replies
- Faster workflow for quick questions

**Cons:**
- Limited context (can't see full history inline)
- Might encourage low-quality messages

**Complexity:** Low-Medium

---

#### 4. Agent Activity Status Cards

**Source:** This conversation  
**Parallel:**
- **openclaw:** Session activity indicator (typing, thinking, idle)
- **hermes:** Agent status in UI (running, idle, error)
- **Cursor:** Agent status in sidebar (thinking, reading, writing)

**Implementation:**
- Add `status` field to agent card (idle/running/working)
- Show last activity timestamp
- Color-coded status dot (green=active, gray=idle, red=error)

**Pros:**
- See who's active at a glance
- Debug stuck agents quickly

**Cons:**
- Requires monitoring to report status (monitor already does this)
- Status can be stale

**Complexity:** Low (already have monitor data)

---

### P3 — Lower Priority

#### 5. Multi-Agent Activity Feed (Dashboard Homepage)

**Source:** This conversation  
**Parallel:**
- **openclaw:** Dashboard homepage shows all active sessions
- **hermes:** Main view shows all agent states
- **Raycast:** Quick access to recent conversations

**Implementation:**
- Add `/` homepage showing:
  - Active agents with status
  - Recent messages (last 10 across all agents)
  - Quick actions (spawn, send to any agent)

**Pros:**
- Single view of everything
- Better than switching between pages

**Cons:**
- Duplicates info available elsewhere
- More UI to maintain

**Complexity:** Medium

---

#### 6. Agent-to-Agent Handoff UI

**Source:** `plans/active/agent-to-agent-communication.md`  
**Parallel:**
- **openclaw:** Delegate tool UI for spawning sub-agents
- **hermes:** Delegate UI for task assignment
- **Linear:** "@mention" in comments triggers handoff

**Implementation:**
- In composer: "@" autocomplete for other agents
- Shows agent cards in dropdown
- Creates inbox row with `from_kind='agent'`

**Pros:**
- Visible way to hand off work
- Leverages existing a2a plan

**Cons:**
- Depends on a2a plan being complete
- Complex permissions model

**Complexity:** High (depends on other plan)

---

#### 7. Sound Notifications

**Source:** User request in conversation  
**Parallel:**
- **Slack:** Notification sounds for mentions
- **Linear:** Sound options in settings

**Implementation:**
- Browser notification API
- Sound file for new inbox message
- User preference to enable/disable

**Pros:**
- Audibly alerts when working elsewhere
- Familiar pattern

**Cons:**
- Browser permissions needed
- Annoying if overused
- Not useful if always in dashboard tab

**Complexity:** Low

---

#### 8. Email Digest Option

**Source:** Generic best practice  
**Parallel:**
- **Notion:** Weekly digest of activity
- **GitHub:** Email notifications configurable

**Implementation:**
- Scheduled job (cron) that emails digest
- Daily/weekly summary of inbox activity

**Pros:**
- For operators who don't live in dashboard
- Archived record

**Cons:**
- Email infrastructure needed
- Another place for notifications

**Complexity:** Medium

---

## Feature Comparison Matrix

| Feature | Impact | Complexity | Depends On | Notes |
|---------|--------|------------|------------|-------|
| Live Inbox Updates | High | Medium | - | P1 |
| Global Badge | High | Low | - | P1 |
| Quick Compose | Medium | Low-Medium | - | P2 |
| Activity Status | Medium | Low | monitor | P2 |
| Activity Feed | Medium | Medium | - | P3 |
| A2A Handoff UI | High | High | a2a plan | P3 |
| Sound Notifications | Low | Low | - | P3 |
| Email Digest | Low | Medium | - | P3 |

---

## Recommendations

### Immediate (Next Sprint)

1. **Live Inbox Updates** - Highest impact, makes dashboard feel responsive
2. **Global Badge** - Quick win, complements live updates

### Short-term (1-2 Sprints)

3. **Activity Status Cards** - Low effort, valuable debug info
4. **Quick Compose** - Nice-to-have workflow improvement

### Later (After A2A Plan)

5. **Agent-to-Agent Handoff UI** - Depends on `agent-to-agent-communication.md`
6. **Multi-Agent Activity Feed** - Depends on live updates being stable

---

## Related Documentation

- `docs/mailbox-architecture.md` - Inbox/outbox architecture
- `plans/active/dashboard.md` - Core dashboard shipped features
- `plans/active/dashboard-gaps.md` - G.1, G.2 shipped (inbox, conversations)
- `plans/active/dashboard-cost-sse.md` - SSE infrastructure for live updates
- `plans/active/agent-to-agent-communication.md` - A2A handoff (future)
- `/openclaw/docs/` - openclaw dashboard reference
- `hermes/web/ui/` - hermes-agent UI reference

---

## openclaw / hermes UI Reference

### openclaw (`../openclaw/`)

- **Session list sidebar:** All agents/workspaces in left nav
- **Real-time updates:** WebSocket connection per session
- **Delegate UI:** Modal for spawning sub-agents with model selection
- **Activity indicators:** "thinking", "idle", "error" states

### hermes-agent (`hermes/web/ui/`)

- **Main dashboard:** Grid of agent cards with status
- **SSE for updates:** Server-sent events for live data
- **Delegate tool:** UI for task handoff
- **Memory view:** Attached/detached memory visualization