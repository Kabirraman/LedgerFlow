/**
 * The browser-side API client.
 *
 * Every request goes to /api/proxy/… on this app's own origin. The server-side
 * proxy attaches the bearer token from the httpOnly session cookie, so nothing in
 * this file — or anywhere else that runs in the browser — ever touches a
 * credential (SRS 19.1).
 *
 * All errors arrive as ApiError. The Go API uses one error envelope for every
 * failure (internal/httpapi/respond.go), and this file unwraps exactly that shape,
 * so a screen has one thing to catch and one place to read a message from.
 */

import type {
  ApprovalsResponse,
  AuditResponse,
  CaseDetailResponse,
  CasePage,
  CaseStatus,
  CasesResponse,
  CheckoutAbandonResponse,
  CheckoutStartResponse,
  DashboardResponse,
  DatasetsResponse,
  DecisionResponse,
  HealthResponse,
  OpsEventsResponse,
  OpsMetricsResponse,
  PoliciesResponse,
  PolicyUpdateResponse,
  ReanalyzeResponse,
  SessionUser,
  SimulationResponse,
  SimulationsListResponse,
  StrategiesResponse,
  StrategyName,
  SyncResponse,
  VerifyResponse,
  VersionResponse,
} from './types';

/** Where the proxy lives. Same origin, always. */
const PROXY = '/api/proxy';

/**
 * A failure from the API, or from the attempt to reach it.
 *
 * `code` is the API's machine-readable code (`policy_blocked`, `not_found`,
 * `validation_failed`, …) and `details` its per-field messages, which is what lets
 * a form put the API's own complaint next to the field it is about instead of
 * showing one generic banner.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: Record<string, string>;
  readonly requestId: string | null;

  constructor(
    status: number,
    code: string,
    message: string,
    details: Record<string, string> = {},
    requestId: string | null = null,
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
    this.requestId = requestId;
  }

  /** True when the session is gone or was never established. */
  get isUnauthenticated(): boolean {
    return this.status === 401;
  }

  /** True when the caller is authenticated but lacks the role for this route. */
  get isForbidden(): boolean {
    return this.status === 403;
  }

  /** True when the deployment simply does not have this capability wired. */
  get isNotConfigured(): boolean {
    return this.code === 'not_configured' || this.code === 'no_gateway';
  }
}

interface ErrorEnvelope {
  error?: {
    code?: unknown;
    message?: unknown;
    details?: unknown;
  };
}

function readDetails(raw: unknown): Record<string, string> {
  if (!raw || typeof raw !== 'object') return {};
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof v === 'string') out[k] = v;
  }
  return out;
}

export type QueryValue = string | number | boolean | undefined | null;

/** Builds a query string, dropping empty values so `?status=` never reaches the API. */
export function buildQuery(params: Record<string, QueryValue> = {}): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue;
    search.set(key, String(value));
  }
  const s = search.toString();
  return s ? `?${s}` : '';
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  query?: Record<string, QueryValue>;
  signal?: AbortSignal;
}

/**
 * Issues one request and returns the parsed body, or throws ApiError.
 *
 * The error envelope is checked regardless of status code. The API deliberately
 * answers a duplicate submission with 200 and an envelope saying so, and a client
 * that only inspected bodies on 4xx would read that as a success.
 */
export async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query, signal } = opts;
  const url = `${PROXY}/${path.replace(/^\/+/, '')}${buildQuery(query)}`;

  let res: Response;
  try {
    res = await fetch(url, {
      method,
      signal,
      headers: body === undefined ? { Accept: 'application/json' } : { Accept: 'application/json', 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err;
    throw new ApiError(0, 'network_error', 'could not reach the LEDGERFLOW web server');
  }

  const requestId = res.headers.get('x-request-id');
  const text = await res.text();

  let parsed: unknown = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      throw new ApiError(
        res.status,
        'bad_response',
        'the API returned a response this console could not read',
        {},
        requestId,
      );
    }
  }

  const envelope = parsed as ErrorEnvelope | null;
  if (envelope && typeof envelope === 'object' && envelope.error) {
    const code = typeof envelope.error.code === 'string' ? envelope.error.code : 'error';
    const message =
      typeof envelope.error.message === 'string' ? envelope.error.message : 'the request failed';
    throw new ApiError(res.status, code, message, readDetails(envelope.error.details), requestId);
  }

  if (!res.ok) {
    throw new ApiError(res.status, 'error', `the request failed with status ${res.status}`, {}, requestId);
  }

  return parsed as T;
}

// --- session ---

export interface SessionResponse {
  user: SessionUser | null;
  permissions?: { can_operate: boolean; can_review: boolean; can_admin: boolean } | null;
}

/**
 * These three do not go through the proxy: they are this app's own routes, because
 * they are the ones that read and write the session cookie.
 */
export async function fetchSession(signal?: AbortSignal): Promise<SessionResponse> {
  const res = await fetch('/api/session', {
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
    cache: 'no-store',
    signal,
  });
  if (!res.ok) return { user: null };
  return (await res.json()) as SessionResponse;
}

export async function login(email: string, password: string): Promise<SessionUser> {
  const res = await fetch('/api/session', {
    method: 'POST',
    headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({ email, password }),
  });
  const text = await res.text();
  let parsed: unknown = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      throw new ApiError(res.status, 'bad_response', 'the login response could not be read');
    }
  }
  const envelope = parsed as ErrorEnvelope | null;
  if (envelope?.error) {
    const code = typeof envelope.error.code === 'string' ? envelope.error.code : 'error';
    const message =
      typeof envelope.error.message === 'string' ? envelope.error.message : 'sign in failed';
    throw new ApiError(res.status, code, message, readDetails(envelope.error.details));
  }
  if (!res.ok) throw new ApiError(res.status, 'error', 'sign in failed');
  const user = (parsed as { user?: SessionUser | null } | null)?.user;
  if (!user) throw new ApiError(502, 'bad_response', 'the login response contained no user');
  return user;
}

export async function logout(): Promise<void> {
  await fetch('/api/session', { method: 'DELETE', credentials: 'same-origin' });
}

// --- typed endpoints ---

/**
 * One function per API route.
 *
 * Screens call these rather than building paths, so a renamed route is a
 * compile-time break in one file instead of a 404 discovered by a reviewer.
 */
export const api = {
  health: (signal?: AbortSignal) => request<HealthResponse>('health', { signal }),

  version: (signal?: AbortSignal) => request<VersionResponse>('version', { signal }),

  dashboard: (signal?: AbortSignal) => request<DashboardResponse>('dashboard/summary', { signal }),

  cases: (query: Record<string, QueryValue>, signal?: AbortSignal) =>
    request<CasesResponse>('cases', { query, signal }),

  caseDetail: (id: string, signal?: AbortSignal) =>
    request<CaseDetailResponse>(`cases/${id}`, { signal }),

  reanalyze: (id: string) => request<ReanalyzeResponse>(`cases/${id}/reanalyze`, { method: 'POST' }),

  verifyCase: (id: string) => request<VerifyResponse>(`cases/${id}/verify`, { method: 'POST' }),

  approve: (id: string, note: string) =>
    request<DecisionResponse>(`cases/${id}/approve`, { method: 'POST', body: { note } }),

  /** The API requires a non-empty note here: a rejection with no reason is not a reviewable record. */
  reject: (id: string, note: string) =>
    request<DecisionResponse>(`cases/${id}/reject`, { method: 'POST', body: { note } }),

  approvals: (query: Record<string, QueryValue>, signal?: AbortSignal) =>
    request<ApprovalsResponse>('approvals', { query, signal }),

  audit: (caseId: string, signal?: AbortSignal) =>
    request<AuditResponse>(`audit/${caseId}`, { signal }),

  strategies: (signal?: AbortSignal) => request<StrategiesResponse>('analytics/strategies', { signal }),

  opsMetrics: (signal?: AbortSignal) => request<OpsMetricsResponse>('ops/metrics', { signal }),

  opsEvents: (limit: number, signal?: AbortSignal) =>
    request<OpsEventsResponse>('ops/events', { query: { limit }, signal }),

  datasets: (signal?: AbortSignal) => request<DatasetsResponse>('datasets', { signal }),

  simulations: (limit: number, signal?: AbortSignal) =>
    request<SimulationsListResponse>('simulations', { query: { limit }, signal }),

  simulation: (id: string, signal?: AbortSignal) =>
    request<SimulationResponse>(`simulations/${id}`, { signal }),

  runSimulation: (body: {
    strategy?: StrategyName;
    baseline?: StrategyName;
    version?: string;
    seed?: number;
    size?: number;
    evaluate?: boolean;
  }) => request<SimulationResponse>('simulations/run', { method: 'POST', body }),

  policies: (signal?: AbortSignal) => request<PoliciesResponse>('policies', { signal }),

  updatePolicy: (body: Record<string, unknown>) =>
    request<PolicyUpdateResponse>('policies', { method: 'PUT', body }),

  syncPayments: (hours: number, count: number) =>
    request<SyncResponse>('sync/payments', { method: 'POST', query: { hours, count } }),

  startCheckout: (body: {
    email: string;
    name?: string;
    contact?: string;
    cart_amount: number;
    item_count?: number;
  }) => request<CheckoutStartResponse>('demo/checkout', { method: 'POST', body }),

  checkoutActivity: (id: string) =>
    request<{ session: CheckoutStartResponse['session'] }>(`demo/checkout/${id}/activity`, {
      method: 'POST',
    }),

  abandonCheckout: (id: string) =>
    request<CheckoutAbandonResponse>(`demo/checkout/${id}/abandon`, { method: 'POST' }),

  convertCheckout: (id: string) =>
    request<{ session: CheckoutStartResponse['session']; note?: string }>(
      `demo/checkout/${id}/convert`,
      { method: 'POST' },
    ),
};

/** Re-exported for screens that narrow a page's items without importing types twice. */
export type { CasePage, CaseStatus };
