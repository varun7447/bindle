# Plan: SOC 2

Status: proposal, not started. Nothing here is a claim of compliance.

## Goal

A SOC 2 Type I report (controls designed and in place at a point in time), then Type II (controls operating over a 3 to 12 month window), covering Security, plus Confidentiality and Availability. Larger customers ask for Type II before sending production traces.

## What the codebase already gives us

Evidence an auditor will ask for and where it lives:

| Control area | Already in place |
|---|---|
| Logical access, tenant isolation | Tenant only from the authenticated key or session; PostgreSQL row-level security; per-tenant OpenSearch filters and routing; tests for each |
| Least privilege | Separate database roles per service (api, ingest, processor, alerts); OpenSearch writer/reader users; scoped IAM per task |
| Encryption | TLS everywhere (HTTPS-only ALB, OpenSearch, RDS); encryption at rest for RDS, OpenSearch, S3 |
| Data minimization | Redaction on by default, twice; secrets dropped; content capture can be turned off per workspace; 30-day retention |
| Secrets | AWS Secrets Manager; API keys stored as SHA-256 hashes only |
| Change management | Code review on every change, tests run before merge |

## What's missing

1. **Policies** (written, approved, reviewed yearly): information security, access control, incident response, change management, vendor management, business continuity, data retention and deletion.
2. **Audit logging**: who in a workspace created or revoked keys, changed SDK settings or alert channels; and Bindle staff access to customer data. Needs a `control.audit_log` table (append-only role) and a console page.
3. **Customer data deletion**: delete a workspace's traces, payloads and control-plane rows on request, with a record that it happened.
4. **Monitoring and alerting** on the platform itself (CloudWatch alarms for ingest errors, queue lag, database health) with an on-call rota.
5. **Backups and restore tests**: RDS automated backups are on; restore needs to be tested and documented. OpenSearch snapshots to S3.
6. **CI enforcement**: branch protection, required reviews and required tests in GitHub; dependency and vulnerability scanning.
7. **People controls**: background checks, security training, onboarding/offboarding checklists.
8. **Penetration test** by an independent firm before the Type I audit.

## Approach

Choose an audit firm and, optionally, a compliance-automation tool (evaluate whether it is needed; it is not required for SOC 2). Close gaps 1 to 6 in code and docs first, since they're also good engineering. Then run the Type I audit, and start the Type II observation window right after it.

## Estimate

Engineering work for gaps 2 to 6: about 4 to 6 weeks. Policies and people controls: ongoing, owned by whoever runs security. Audit timing depends on the firm.
