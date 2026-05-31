import { PageHeader } from "@/components/ds";
import { useAuth } from "@/auth/useAuth";

export default function Home() {
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  void isAdmin; // used by data widgets in later tasks
  return (
    <div className="home-page">
      <PageHeader title="Overview" subtitle="Your relay at a glance." />
    </div>
  );
}
