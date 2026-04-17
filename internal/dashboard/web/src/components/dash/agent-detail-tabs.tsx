"use client";

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

// The tabbed detail surface for a single agent. useDashStream stays
// mounted while this page is active so SSE events keep the lists
// and timeline current.
export function AgentDetailTabs({ agentId }: { agentId: string }) {
  useDashStream();

  return (
    <Tabs defaultValue="conversation" className="w-full">
      <TabsList data-testid="agent-detail-tabs" className="grid grid-cols-3">
        <TabsTrigger data-testid="tab-conversation" value="conversation">
          Chat
        </TabsTrigger>
        <TabsTrigger data-testid="tab-inbox" value="inbox">
          Inbox
        </TabsTrigger>
        <TabsTrigger data-testid="tab-outbox" value="outbox">
          Outbox
        </TabsTrigger>
      </TabsList>
      <TabsContent value="conversation">
        <ConversationView agentId={agentId} />
      </TabsContent>
      <TabsContent value="inbox">
        <InboxList agentId={agentId} />
      </TabsContent>
      <TabsContent value="outbox">
        <OutboxList agentId={agentId} />
      </TabsContent>
    </Tabs>
  );
}
