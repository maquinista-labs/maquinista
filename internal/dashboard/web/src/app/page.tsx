import { redirect } from "next/navigation";

// Root of the dashboard redirects to /agents (the default landing).
// Keeps `/` a stable entry point while letting the (dash) route
// group own the header + bottom nav.
export default function Home() {
  redirect("/agents");
}
