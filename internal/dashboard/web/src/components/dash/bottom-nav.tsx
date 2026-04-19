"use client";

import { Bot, Inbox, MessageSquare, CalendarClock } from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ComponentType, SVGProps } from "react";

import { cn } from "@/lib/utils";
import { useInboxCount } from "@/lib/hooks";

// Bottom nav: four touch targets, min-height 44 px to meet Apple's
// HIG. Each tab's label is shown under the icon for discoverability
// (iOS-style) and stays within a 12-char budget so the layout
// doesn't break on narrow phones.
type Tab = {
  href: string;
  label: string;
  Icon: ComponentType<SVGProps<SVGSVGElement>>;
  testId: string;
};

const tabs: Tab[] = [
  { href: "/agents", label: "Agents", Icon: Bot, testId: "nav-agents" },
  { href: "/inbox", label: "Inbox", Icon: Inbox, testId: "nav-inbox" },
  {
    href: "/conversations",
    label: "Chats",
    Icon: MessageSquare,
    testId: "nav-conversations",
  },
  { href: "/jobs", label: "Jobs", Icon: CalendarClock, testId: "nav-jobs" },
];

export function BottomNav() {
  const pathname = usePathname();
  const { data: inboxCount = 0 } = useInboxCount();

  return (
    <nav
      data-testid="bottom-nav"
      aria-label="Primary"
      className="sticky bottom-0 z-20 grid grid-cols-4 border-t border-border/60 bg-background/90 backdrop-blur supports-[backdrop-filter]:bg-background/70"
      style={{ paddingBottom: "env(safe-area-inset-bottom)" }}
    >
      {tabs.map(({ href, label, Icon, testId }) => {
        const active = pathname === href || pathname.startsWith(`${href}/`);
        const isInbox = href === "/inbox";
        return (
          <Link
            key={href}
            href={href}
            data-testid={testId}
            aria-current={active ? "page" : undefined}
            className={cn(
              "flex min-h-[52px] flex-col items-center justify-center gap-0.5 text-xs",
              active
                ? "text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <span className="relative inline-flex">
              <Icon className="h-5 w-5" aria-hidden />
              {isInbox && inboxCount > 0 && (
                <span
                  data-testid="nav-inbox-badge"
                  className="absolute -right-1.5 -top-1.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-red-500 px-0.5 text-[10px] font-bold leading-none text-white"
                >
                  {inboxCount > 99 ? "99+" : inboxCount}
                </span>
              )}
            </span>
            <span className="leading-none">{label}</span>
          </Link>
        );
      })}
    </nav>
  );
}
