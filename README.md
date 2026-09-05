# LedgerFlow

### Autonomous Revenue Recovery Operating System

LedgerFlow is an autonomous revenue recovery platform designed to identify revenue at risk, diagnose the underlying cause, recommend recovery interventions, execute approved actions, and verify outcomes while maintaining strict policy controls and a complete audit trail.

> The sections below (numbered 15–29) are excerpted from the full Software Requirements Specification (`Ledgerflow_SRS.docx`), which also covers architecture, data model, and API contracts (sections 1–14).

**See also:** [`ARCHITECTURE.md`](./ARCHITECTURE.md) for a diagram of the recovery pipeline and the safety boundaries it enforces.
---

## Getting Started

### Prerequisites

* Go 1.23 or newer
* Node.js 20.11 or newer
* PostgreSQL 14 or newer (or use the Docker Compose setup below, which includes it)

### Option A — Docker Compose (fastest)

```bash
git clone https://github.com/Kabirraman/LedgerFlow.git
cd LedgerFlow
docker compose up --build
```

* Frontend: http://localhost:3000
* Backend API: http://localhost:8080
* Postgres: localhost:5432 (user/pass/db: `ledgerflow`)

The backend automatically runs migrations, seeds a demo dataset, and creates a default admin account on first boot (see below).

### Option B — Run locally without Docker

**1. Start Postgres** and create a database, or point `DATABASE_URL` at one you already have. The default the backend expects is:

```
postgres://ledgerflow:ledgerflow@localhost:5432/ledgerflow?sslmode=disable
```

**2. Backend**

```bash
cd backend
go run ./cmd/server
```

No `.env` file is required for local development — every setting has a safe default (see `internal/config/config.go`). Migrations run automatically and a demo dataset is seeded on first boot. If you want to override anything, copy the table below into a `backend/.env` file.

| Variable | Default (local) | Notes |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://ledgerflow:ledgerflow@localhost:5432/ledgerflow?sslmode=disable` | |
| `PORT` | `8080` | |
| `JWT_SECRET` | dev-only fixed string | **required** and must be ≥32 chars once `APP_ENV != local` |
| `SEED_ADMIN_EMAIL` | `admin@ledgerflow.test` | |
| `SEED_ADMIN_PASSWORD` | `ledgerflow` | **required** once `APP_ENV != local` |
| `RAZORPAY_KEY_ID` / `RAZORPAY_KEY_SECRET` | unset | leave unset to run against the built-in sandbox gateway instead of real Razorpay test mode |
| `RAZORPAY_WEBHOOK_SECRET` | unset | required to accept real Razorpay webhooks |
| `RAZORPAY_MODE` | `test` | the server refuses to boot with anything else, and refuses `rzp_live` key ids outright |
| `GEMINI_API_KEY` | unset | leave unset to fall back to the deterministic rules engine instead of calling Gemini |

**3. Frontend**

```bash
cd frontend
cp .env.example .env.local
npm install
npm run dev
```

Visit http://localhost:3000 and log in with the seeded admin account:

* **Email:** `admintest@gmail.com`
* **Password:** `admintest123`

> These are local-development-only defaults enforced by the backend — see `internal/config/config.go`. They are rejected outright the moment `APP_ENV` is anything other than `local`.

### Running tests

```bash
cd backend && go test ./...
cd frontend && npm run lint && npm run typecheck
```

---

## 15.3 Example Planner Response

```json
{
  "case_id": "REV-0182",
  "recommended_action": "payment_link",
  "recovery_probability": 0.83,
  "expected_recovery": 69720,
  "reason_codes": [
    "high_intent",
    "repeat_customer",
    "single_failure"
  ],
  "alternatives": [
    "retry"
  ],
  "stop_condition": "customer_paid OR action_count >= 2"
}
```

---

# 16. Frontend / UX Requirements

## 16.1 Dashboard

The dashboard should provide a concise overview of the revenue recovery pipeline.

* Show **Revenue at Risk**, **Recovered**, **Recovery Rate**, and **Automated Actions** as primary KPI cards.
* Show the recovery funnel:

```text
Identified → Diagnosed → Actioned → Recovered
```

* Show a live activity feed for:

  * Executed actions
  * Blocked actions
  * Escalated actions
* Provide filters by:

  * Scenario
  * Customer segment
  * Risk
  * Action type

---

## 16.2 Case Detail

Each recovery case should provide:

* Case amount
* Risk score
* Expected recovery
* Current status
* Why-at-risk evidence
* Diagnosis confidence
* Planner decision
* Alternative actions
* Policy checks with `PASS`, `BLOCK`, or `ESCALATE` labels
* Action timeline
* External Razorpay resource IDs
* Recovery outcome
* Recovered amount
* Full audit log accessible on demand

---

## 16.3 Approval Queue

The approval queue should prioritize:

* High-value cases
* Low-confidence cases

Operators should have:

* One-click **Approve / Reject**
* A required rejection reason
* Full visibility into the proposed action

> No hidden or pre-executed actions may occur before approval.

---

## 16.4 Simulation Lab

The Simulation Lab should allow operators to:

* Select a dataset version
* Select a baseline policy
* Select the LEDGERFLOW strategy
* Run simulations without external API calls
* View:

  * Recovery amount
  * Recovery rate
  * Interventions
  * Escalations
  * Safe stops
  * Errors
* Compare baseline performance against LEDGERFLOW

---

# 17. Simulation Lab & Benchmarking

## 17.1 Benchmark Dataset

The target benchmark consists of **200 synthetic cases**, with at least **100 cases available for the buildathon demo**.

Suggested distribution:

| Scenario              |   Cases |
| --------------------- | ------: |
| Payment failures      |      70 |
| Checkout abandonment  |      40 |
| Overdue invoices      |      40 |
| Subscription failures |      30 |
| Edge cases            |      20 |
| **Total**             | **200** |

---

## 17.2 Case Attributes

| Attribute          | Examples                                                    |
| ------------------ | ----------------------------------------------------------- |
| **Transaction**    | Amount, payment method, timestamp, status, failure code     |
| **Customer**       | Segment, prior success rate, lifetime value, recency        |
| **Behavior**       | Checkout views, time since abandonment, attempt count       |
| **History**        | Previous recoveries, failed interventions, reminder count   |
| **Ground Truth**   | Recoverable flag, benchmark-best action, expected outcome   |
| **Policy Context** | Amount threshold, cooldown, retry limit, approval threshold |

---

## 17.3 Baselines

| Baseline                | Rule                                                                      |
| ----------------------- | ------------------------------------------------------------------------- |
| **Retry-everything**    | Every eligible payment failure gets a retry up to the maximum retry limit |
| **Reminder-everything** | Every abandoned or overdue case receives a reminder                       |
| **Static heuristic**    | Action is selected using fixed `if/else` rules based on failure type      |
| **LEDGERFLOW**          | Four-agent decisioning + policy engine + strategy metrics                 |

---

## 17.4 Required Output

The benchmark should report:

```text
Cases processed:             200
Revenue at risk:             ₹X
Eligible opportunities:      X
Actions executed:            X
Recovered amount:            ₹Y
Recovery rate:               Y/X
Escalated:                   X
Stopped safely:              X
Policy violations:           0
Baseline recovered amount:   ₹B
LEDGERFLOW uplift:           (Y-B)/B
```

---

# 18. Analytics & Metrics

## 18.1 Business Metrics

* Revenue at Risk
* Expected Recoverable Revenue
* Actual Recovered Amount
* Recovery Rate
* Average Recovery Time
* Revenue Recovered per Intervention
* Escalation Rate
* Unresolved Revenue

---

## 18.2 Agent Metrics

### Detection

* Precision and recall on benchmark labels

### Diagnosis

* Root-cause classification accuracy

### Intervention Planner

* Intervention selection accuracy

### Confidence

* Compare predicted recovery probability against observed outcomes

### Explainability

* Unsupported-claim rate
* Evidence coverage

---

## 18.3 Operational Metrics

Track:

* Webhook verification failures
* Duplicate-event rate
* Action execution latency
* API failure rate
* Policy blocks
* Escalation counts
* Agent response latency
* Token usage and cost per case

---

# 19. Security & Privacy

## 19.1 Secrets

The following secrets must remain server-side:

* Razorpay key secret
* Gemini API key
* Webhook secret

Development secrets should be supplied through environment variables or a dedicated secret manager.

> Secrets must never be committed to the repository or exposed to the browser.

---

## 19.2 AI Safety

LLM outputs are treated as **untrusted recommendations**.

LedgerFlow enforces the following controls:

* No direct tool access from the model to money-moving endpoints
* All actions use allow-listed enums
* All amounts are derived from trusted database/API data rather than generated text
* High-value or uncertain cases require human approval
* `UNKNOWN` and `NO_ACTION` are first-class states
* Every external action is auditable

---

## 19.3 Webhook Security

The webhook endpoint must validate the webhook signature against the **raw request body** using the configured secret before parsing or acting on the event.

---

## 19.4 Data Minimization

The system should store only the payment and customer information required for:

* Recovery logic
* Operational processing
* Auditing

Sensitive payment instrument details should not be copied into the application database unless explicitly required by an approved API use case.

---

# 20. Reliability & Failure Handling

## 20.1 Duplicate Events

Incoming webhook event IDs and action idempotency keys must prevent duplicate external side effects.

---

## 20.2 Lost Response

If an external action may have succeeded but the response is ambiguous, LedgerFlow must **not immediately create another action**.

Instead:

```text
Ambiguous Response
       ↓
Reconcile External State
       ↓
Continue OR Escalate
```

---

## 20.3 External API Errors

| Failure                  | Expected Behavior                                                                    |
| ------------------------ | ------------------------------------------------------------------------------------ |
| **Timeout**              | Retry once using the same idempotency key if safe; then verify before retrying again |
| **4xx validation error** | Do not retry blindly; record the error and escalate or close                         |
| **5xx transient error**  | Bounded retry with backoff; stop after retry budget                                  |
| **Webhook delayed**      | Case remains `VERIFYING`; poll/reconcile according to policy                         |
| **Out-of-order event**   | Use entity timestamps/state checks and ignore stale transitions                      |
| **Conflicting state**    | Mark `UNKNOWN`, prevent action, and escalate for review                              |

---

## 20.4 Graceful Agent Failure

If an AI agent:

* Times out
* Returns invalid JSON
* Produces low-confidence output

the system must **never fail open**.

It should transition to an appropriate safe state:

```text
Deterministic Fallback
        OR
     NO_ACTION
        OR
     ESCALATE
```

depending on the workflow.

---

# 21. Non-Functional Requirements

| ID          | Requirement                                                                         | Priority | Acceptance / Notes                 |
| ----------- | ----------------------------------------------------------------------------------- | -------- | ---------------------------------- |
| **NFR-001** | Security: all secrets server-side; authenticated operator endpoints                 | M        | No secrets in browser bundle       |
| **NFR-002** | Reliability: action execution is idempotent                                         | M        | Duplicate tests pass               |
| **NFR-003** | Auditability: every side effect has case/decision/policy linkage                    | M        | 100% action traceability           |
| **NFR-004** | Explainability: users see evidence and reason codes, not hidden chain-of-thought    | M        | No private model reasoning exposed |
| **NFR-005** | Dashboard API p95 response under 500 ms for cached/DB summaries in demo environment | S        | Measured locally/deployed          |
| **NFR-006** | Common-case end-to-end recommendation under 8 s, excluding external webhook delay   | S        | Measured during demo               |
| **NFR-007** | Maintainability: clear module boundaries and typed interfaces                       | M        | Repository structure reviewed      |
| **NFR-008** | Testability: simulation mode can reproduce fixed dataset results                    | M        | Dataset and seed versioned         |
| **NFR-009** | Observability: structured logs include `case_id` and `action_id`                    | S        | Queryable logs                     |
| **NFR-010** | Usability: operators understand why an action was taken within one minute           | S        | User test / internal review        |

---

# 22. Testing Strategy

## 22.1 Unit Tests

Test:

* Risk-score calculations
* Expected recovery calculations
* Policy rule evaluation
* Action allow-list validation
* Idempotency key generation
* Webhook signature verification
* Case state transitions

---

## 22.2 Integration Tests

Test:

* Razorpay test-mode payment-link creation and verification
* Webhook receipt and signature validation
* Invoice lifecycle where available
* Subscription test workflow where available
* API timeout and retry behavior
* Duplicate event delivery

---

## 22.3 AI Evaluation

| Agent          | Evaluation Method                                                             | Minimum Target                            |
| -------------- | ----------------------------------------------------------------------------- | ----------------------------------------- |
| **Detection**  | Held-out labeled set; precision/recall on at-risk classification              | ≥ 0.80 precision; ≥ 0.75 recall target    |
| **Diagnosis**  | Root-cause classification + evidence coverage                                 | ≥ 0.80 accuracy                           |
| **Planner**    | Compare selected action against benchmark-best action / accepted alternatives | ≥ 0.75 accuracy                           |
| **Executor**   | Schema, allow-list, policy and execution tests                                | 0 unauthorized actions; 100% valid schema |
| **End-to-End** | Recovery amount compared with baseline                                        | Positive uplift over chosen baseline      |

---

## 22.4 Safety Tests

LedgerFlow should verify that:

* Prompt injection text embedded in a customer note cannot alter action permissions
* An unavailable LLM-proposed action is rejected
* An LLM-proposed amount different from the trusted transaction amount is rejected
* Amounts exceeding configured thresholds are escalated
* Retry counts exceeding limits cause the system to stop
* Duplicate action requests return the existing result
* Simulation requests never call Razorpay

---

# 23. Deployment & DevOps

## 23.1 Local Development

| Component | Technology                                |
| --------- | ----------------------------------------- |
| Frontend  | Next.js development server                |
| Backend   | Go + Gin                                  |
| Database  | PostgreSQL                                |
| AI        | Gemini API                                |
| Payments  | Razorpay Test Mode                        |
| Webhooks  | Approved localhost tunnel for development |

---

## 23.2 Docker

The recommended demo deployment uses Docker Compose or a small managed deployment with separate:

* Frontend
* Backend
* PostgreSQL

services.

The webhook endpoint must be reachable from Razorpay test-mode infrastructure.

---

## 23.3 CI Checks

Recommended CI checks include:

```bash
go test ./...
```

Along with:

* Frontend type checking
* Frontend linting
* Database migration validation
* API contract tests
* Benchmark regression tests
* Secret scanning

---

## 23.4 Environment Separation

| Environment        | Purpose                   | Keys                                               |
| ------------------ | ------------------------- | -------------------------------------------------- |
| **Local**          | Developer implementation  | Test keys only                                     |
| **Staging / Demo** | Judging environment       | Dedicated test keys and webhook secret             |
| **Production**     | Not part of hackathon MVP | Live keys must never be used in the prototype demo |

---

# 24. Phased Implementation Plan

| Phase                         | Deliverables                                                                           | Exit Criteria                                     |
| ----------------------------- | -------------------------------------------------------------------------------------- | ------------------------------------------------- |
| **P0 — Foundation**           | Repo, authentication, PostgreSQL, environment configuration, Razorpay test credentials | Backend and DB boot; health endpoint works        |
| **P1 — Ingestion**            | Payment sync, webhook endpoint, signature validation, event store                      | Events visible in DB with deduplication           |
| **P2 — Core Case Engine**     | Detection score, `risk_cases`, queue and case API                                      | Risk cases generated from test data               |
| **P3 — Four Agents**          | Detection, Diagnosis, Planner, Executor contracts + Gemini structured output           | One case traverses the full agent pipeline safely |
| **P4 — Policy + Safety**      | Rules, approval queue, stopping logic, idempotency                                     | Forbidden actions blocked in tests                |
| **P5 — First Real Recovery**  | Payment Link action + verification                                                     | End-to-end test recovery demonstrated             |
| **P6 — Additional Workflows** | Checkout abandonment + invoices; subscription extension if time                        | At least one additional recovery workflow works   |
| **P7 — Benchmark**            | 200 synthetic cases, baseline, LEDGERFLOW comparison                                   | Metrics reproducible and exportable               |
| **P8 — UX Polish**            | Dashboard, case timeline, simulation lab, analytics                                    | Five-minute demo path is smooth                   |
| **P9 — Hardening**            | Failure handling, duplicate events, prompt attacks, documentation and pitch            | Acceptance criteria are green                     |

---

# 25. Demo & Pitch Acceptance Flow

## 25.1 Five-Minute Demo

| Time          | Action                                             | Judge Takeaway                            |
| ------------- | -------------------------------------------------- | ----------------------------------------- |
| **0:00–0:30** | Show ₹X revenue at risk and problem statement      | Clear business value                      |
| **0:30–1:15** | Open a real/test-mode case                         | Agent identifies a meaningful opportunity |
| **1:15–2:00** | Show diagnosis and evidence                        | AI is grounded, not decorative            |
| **2:00–2:45** | Show planner decision + policy checks              | Action is explainable and bounded         |
| **2:45–3:30** | Execute Payment Link / recovery action             | Agent closes the loop                     |
| **3:30–4:00** | Verify success and show recovered amount           | Measurable value                          |
| **4:00–4:30** | Run 100/200-case benchmark                         | Scales beyond a cherry-picked demo        |
| **4:30–5:00** | Show audit trail + graceful failure + architecture | Reliability and engineering depth         |

---

## 25.2 Anti-Cherry-Picking Rule

The following must be versioned:

* Benchmark dataset
* Random seed
* Baseline rules
* Evaluation code

Demo numbers must be clearly labeled as either:

* Test-mode results
* Synthetic benchmark results

> No synthetic recovery amount should be presented as actual live merchant revenue.

---

# 26. Risks & Mitigations

| Risk                                             | Impact   | Mitigation                                                                                                       |
| ------------------------------------------------ | -------- | ---------------------------------------------------------------------------------------------------------------- |
| Razorpay test API limitations                    | High     | Maintain simulator as a parallel path and verify available endpoints early                                       |
| Overlap with existing Razorpay recovery products | High     | Differentiate through unified multi-scenario decisioning, expected recovery value and policy-aware orchestration |
| LLM hallucination                                | High     | Structured outputs, evidence requirements, deterministic policy and allow-list                                   |
| Duplicate financial action                       | Critical | Idempotency keys, persistent action state and reconciliation before retry                                        |
| Webhook ordering / delay                         | High     | State machine, timestamps, replay buffer and verification polling where appropriate                              |
| Excessive scope                                  | High     | Protect P1–P5; implement optional workflows only after the core loop works                                       |
| Weak benchmark                                   | High     | Version dataset, define ground truth and compare against simple baselines                                        |
| Demo failure due to network                      | High     | Preload test cases, preserve simulation mode and retain recorded API artifacts for explanation                   |

---

# 27. Future Enhancements

* Adaptive policy optimization based on intervention outcomes
* Contextual multi-channel recovery through email, SMS and WhatsApp with consent and merchant policy controls
* Very-high-value B2B promise-to-pay tracking
* Hinglish voice recovery for an approved and clearly bounded use case
* Bandit-style strategy selection after sufficient outcome data
* Merchant-specific recovery probability calibration
* Cross-merchant aggregate learning using privacy-preserving techniques
* Production-grade queues, observability and multi-tenant scaling

---

# 28. Acceptance Criteria

| ID         | Requirement                                                                                  | Priority | Acceptance / Notes                              |
| ---------- | -------------------------------------------------------------------------------------------- | -------- | ----------------------------------------------- |
| **AC-001** | At least one real Razorpay test-mode recovery workflow completes end-to-end                  | M        | Required for demo                               |
| **AC-002** | All four agents execute through typed contracts in the main recovery path                    | M        | Detection → Diagnosis → Planner → Executor      |
| **AC-003** | Policy engine can block and escalate actions deterministically                               | M        | Safety tests pass                               |
| **AC-004** | System shows measured recovered amount on a batch                                            | M        | Simulation benchmark + test-mode cases          |
| **AC-005** | Every external side effect has an audit trail                                                | M        | Case ID and action ID linkage                   |
| **AC-006** | Duplicate webhook and duplicate action tests do not cause double side effects                | M        | Idempotency test passes                         |
| **AC-007** | At least one graceful failure is demonstrated                                                | S        | Example: API failure / policy stop / escalation |
| **AC-008** | Baseline comparison shows measurable improvement or clearly identifies where the agent loses | M        | Honest benchmark                                |
| **AC-009** | Simulation mode cannot call Razorpay APIs                                                    | M        | Architecture and test proof                     |
| **AC-010** | Demo UI explains why an action was selected without exposing private chain-of-thought        | S        | Evidence + reason codes displayed               |

---

# 29. Glossary

| Term                             | Definition                                                                                |
| -------------------------------- | ----------------------------------------------------------------------------------------- |
| **Revenue at Risk**              | Monetary value associated with an event where recovery may still be possible              |
| **Expected Recoverable Revenue** | Revenue at risk multiplied by estimated recovery probability and intervention feasibility |
| **Recovery Case**                | Tracked unit of work containing diagnosis, decision, policy, action and outcome           |
| **Intervention**                 | Bounded action intended to recover revenue, such as a retry, payment link or reminder     |
| **Stopping Rule**                | Deterministic rule that tells the agent to stop acting                                    |
| **Escalation**                   | Transfer of a case to a human reviewer rather than autonomous execution                   |
| **Action Executor**              | Fourth agent / deterministic integration layer responsible for approved side effects      |
| **Simulation Mode**              | Isolated execution path using synthetic data and no external money-moving calls           |
| **Audit Trail**                  | Append-oriented record of relevant decisions, policy checks, actions and outcomes         |
| **Held-out Set**                 | Benchmark cases not used when tuning prompts or heuristics                                |
