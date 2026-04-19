"use client";

import { useSearchParams } from "next/navigation";

import { useDashStream } from "@/lib/sse";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";

import { ConversationView } from "@/components/dash/conversation-view";
import { InboxList } from "@/components/dash/inbox-list";
import { OutboxList } from "@/components/dash/outbox-list";
import { WorkspacesCard } from "@/components/dash/workspaces-card";

// The tabbed detail surface for a single agent. useDashStream stays
// mounted so SSE events keep the lists and timeline current.
//
// Respects ?tab=... and ?conversation=... — `tab=chat` (the URL
// shape used by the top-level Chats feed) maps to the internal
// "conversation" value. When `conversation=<id>` is present, the
// chat pane shows that thread instead of the agent's cross-
// conversation timeline.
export function AgentDetailTabs({ agentId }: { agentId: string }) {
  useDashStream();

  const params = useSearchParams();
  const rawTab = params.get("tab");
  const conversationId = params.get("conversation");
  const tab = rawTab === "chat" ? "conversation" : rawTab;
  const initial =
    tab === "inbox" ||
    tab === "outbox" ||
    tab === "conversation" ||
    tab === "workspaces"
      ? tab
      : "conversation";

  return (
    <Tabs defaultValue={initial} className="w-full">
      <TabsList data-testid="agent-detail-tabs" className="grid grid-cols-4">
        <TabsTrigger data-testid="tab-conversation" value="conversation">
          Chat
        </TabsTrigger>
        <TabsTrigger data-testid="tab-inbox" value="inbox">
          Inbox
        </TabsTrigger>
        <TabsTrigger data-testid="tab-outbox" value="outbox">
          Outbox
        </TabsTrigger>
        <TabsTrigger data-testid="tab-workspaces" value="workspaces">
          Workspaces
        </TabsTrigger>
      </TabsList>
      <TabsContent value="conversation">
        {conversationId ? (
          <ConversationView conversationId={conversationId} liveAgentId={agentId} />
        ) : (
          <ConversationView agentId={agentId} />
        )}
      </TabsContent>
      <TabsContent value="inbox">
        <InboxList agentId={agentId} />
      </TabsContent>
      <TabsContent value="outbox">
        <OutboxList agentId={agentId} />
      </TabsContent>
      <TabsContent value="workspaces">
        <WorkspacesCard agentId={agentId} />
      </TabsContent>
    </Tabs>
  );
}
