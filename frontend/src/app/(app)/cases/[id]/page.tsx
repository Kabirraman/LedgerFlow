import { CaseView } from './case-view';

/**
 * Case detail (SRS 16.2).
 *
 * A server component only so the route parameter can be awaited once and handed to
 * the client view as a plain string; everything below this is client-rendered because
 * it polls.
 */
export default async function CaseDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return <CaseView caseId={id} />;
}
