import { redirect } from 'next/navigation';

/**
 * The root has no content of its own. Middleware has already decided whether there is
 * a session; if there is, the dashboard is where an operator starts (SRS 16.1).
 */
export default function Home() {
  redirect('/dashboard');
}
