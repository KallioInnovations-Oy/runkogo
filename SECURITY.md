# RunkoGO Security Policy

RunkoGO is a zero-dependency Go framework where security is a framework-level concern, not an application-level afterthought. This document explains the security *policy* and the reasoning behind it.

**The conventions themselves live in [CONVENTIONS.md](CONVENTIONS.md)**, which is the source of truth. Each is labelled *Enforced* (the framework guarantees it, with a test that fails if it breaks), *Convention* (you must register or do something), or *Deferred* (out of scope). This document references those IDs rather than restating them, so the two cannot drift — a test fails the build if they do.

The distinction matters most where a protection is middleware you have to register. `DefaultSecurityHeaders()`, `Logger()`, `BodyLimit()`, `RateLimit()`, `CSRF()` and `AllowedHosts()` do nothing until they are in your chain.

## Core Principle

**`runko.New()` with zero configuration produces a secure application.** Weakening any security default requires explicit, documented opt-in. Dangerous configurations cause startup panics, not runtime bugs.

## Security Conventions

### Secure by Default, Explicit Opt-In for Trust

*[CONV-01](CONVENTIONS.md#conv-01--secure-by-default-explicit-opt-in-for-trust) — Enforced.*

The framework never trusts external input without explicit configuration. Forwarding headers (`X-Forwarded-For`) are ignored unless `TrustedProxies` is configured. CORS is denied unless origins are listed. Health check details are hidden unless `HEALTH_DETAIL=true`.

When trusted proxies are configured, all `X-Forwarded-For` header lines are joined before the chain is walked — some proxies append a new line rather than extending the existing one, and reading only the first would let a client inject its own address ahead of the proxy's. An entry that does not parse as an IP terminates the walk and falls back to `RemoteAddr`: everything to the left of a corrupt entry is attacker-reachable.

`X-Real-IP` is **not** read. Configure your proxy to set `X-Forwarded-For`.

### External Input Sanitization

*[CONV-02](CONVENTIONS.md#conv-02--external-input-is-sanitized-before-use) — Enforced.*

All values from HTTP headers, query parameters, and request bodies are validated before use in logging, responses, or internal state. Identifiers accept only `[a-zA-Z0-9_-]` up to 64 characters.

### No Internal Details to Unauthenticated Clients

*[CONV-03](CONVENTIONS.md#conv-03--no-internal-detail-reaches-an-unauthenticated-client) — Enforced API shape; the message content is yours to keep clean.*

Error responses never contain stack traces, hostnames, connection strings, file paths, or raw library errors. `runko.Error` accepts only a public code and public message; use `runko.ErrorLog` to attach an internal error that goes to the server log with request correlation. Health checks expose only pass/fail by name, never error messages, unless explicitly enabled.

### Security Headers Available by Default

*[CONV-04](CONVENTIONS.md#conv-04--security-headers-on-every-response) — Convention to register, Enforced coverage once registered.*

The framework ships `DefaultSecurityHeaders()` as a ready-to-use middleware. Include it in your **root** middleware chain — both the scaffold and example demonstrate it. Every response then includes `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Cache-Control: no-store`, `Referrer-Policy: strict-origin-when-cross-origin`, and `Permissions-Policy`. HSTS requires explicit opt-in.

"Every response" is exact: root middleware wraps the router itself, so 404s, 405s and path-cleaning redirects are covered too. Registering security headers on a `Group` instead covers only that group's matched routes and leaves unmatched requests bare.

### TLS Hardening

When TLS is configured, the server enforces a minimum TLS version via `Options.TLSMinVersion`. The default is TLS 1.2 (broad compatibility); new deployments should prefer `tls.VersionTLS13`. Go's default cipher suites apply — they are safe for TLS 1.2+.

### Bounded Request Headers

`http.Server.MaxHeaderBytes` defaults to 64 KiB via `Options.MaxHeaderBytes` — 16× tighter than Go's 1 MB default. This fits typical browser traffic (cookies + JWT + tracing headers) while rejecting abusive payloads. Raise for SSO-heavy deployments that carry large SAML cookies.

### CSRF Protection for Cookie-Authenticated Apps

*[CONV-14](CONVENTIONS.md#conv-14--csrf-protection-for-cookie-authenticated-apps) — Convention.*

The `CSRF` middleware implements the double-submit-cookie pattern for webapps that rely on cookie authentication. It issues a token cookie on safe methods and requires the value to be echoed in the `X-CSRF-Token` request header on unsafe methods. For purely token-authenticated APIs (Authorization: Bearer …), enable `SkipAuthHeader` to bypass the check — bearer tokens are not sent by browsers automatically.

**Known limitation — subdomain cookie injection.** The middleware stores the CSRF token as a plain cookie (not HMAC-signed against a session). An attacker who controls a sibling subdomain (via XSS on `evil.example.com` or a compromised host) can set the parent-domain cookie via `document.cookie` and defeat the check. The standard mitigation is a signed double-submit cookie tied to the session identifier; RunkoGO does not yet ship session infrastructure, so the signed variant is unavailable. Until it is, compensate at the infrastructure layer: restrict which subdomains can be provisioned, enable HSTS with `includeSubDomains`, and prefer `SameSite=Strict` where the application allows.

### Fail Fast on Dangerous Configuration

*[CONV-05](CONVENTIONS.md#conv-05--dangerous-configuration-fails-at-startup-not-at-runtime) — Enforced.*

CORS wildcard with credentials, invalid trusted proxy entries, empty `AllowedHosts`, and partial TLS configuration all cause startup panics with actionable messages.

### Bounded Resource Consumption

*[CONV-06](CONVENTIONS.md#conv-06--resource-consumption-is-bounded) — Enforced.*

Request bodies: 1 MB default via `BodyLimit`. Request headers: 64 KiB default. Rate limiter: 10 000 client cap. Service client responses: 10 MB cap. Request IDs: 64 character max.

The rate limiter's cap is enforced by **evicting the least recently seen client**, never by refusing an unseen one. Refusing at capacity would invert the control into an attack: a client rotating through cheap addresses could fill the table and lock out every new legitimate visitor, which is cheaper and more effective than the flooding the limiter defends against. The tradeoff is that a determined address-rotator can evict other clients' windows — acceptable, because that degrades enforcement rather than denying service.

IPv6 is bucketed by /64 rather than by full address, since a single client is routinely delegated 2^64 addresses. A shared /64 is therefore limited collectively.

The limiter is **per-instance** — see [CONV-11](CONVENTIONS.md#conv-11--rate-limiting-is-per-instance-not-per-cluster).

### Timeout Everything

*[CONV-07](CONVENTIONS.md#conv-07--everything-has-a-timeout) — Enforced. Note the fixed 30s `WriteTimeout`, which rules out SSE and long downloads.*

HTTP server has read/write/idle timeouts. Service client has per-request timeout with retries. Health checks have per-check timeouts via context deadlines.

### Audit Trail by Default

*[CONV-08](CONVENTIONS.md#conv-08--audit-trail-by-default) — Convention to register, Enforced coverage once registered.*

Every request is logged by the Logger middleware with method, path, status, duration, request ID, and resolved client IP. This includes requests that match no route: an attacker probing for endpoints leaves a record, which was previously the largest blind spot in the trail. Authenticated user identity is logged at the handler level via `LogWithContext()` because auth middleware runs inside the Logger's scope and the enriched context is not available to the outer middleware log line.

## Privacy Conventions

### No Sensitive Data in Logs

*[CONV-09](CONVENTIONS.md#conv-09--privacy-sensitive-data-stays-out-of-logs-and-urls) — Enforced.*

The logger never captures request bodies, response bodies, Authorization header values, or cookie values.

### No Sensitive Data in URLs

Query strings are not logged by default. When enabled via `LoggerWithConfig`, sensitive parameters (`token`, `key`, `password`, `secret`, `api_key`, `access_token`, `refresh_token`, `session`, `csrf`) are automatically redacted.

### Request ID ≠ Session Identifier

Request IDs are generated fresh per request, never derived from user identity, and must not be used to track users across requests. The generator uses non-cryptographic randomness because request IDs are correlation aids, not security tokens.

### Response Headers Reveal Minimum

No `Server` header is set. The framework does not identify itself in responses.

### Data Minimization in Error Responses

Error responses never echo user input back. Messages are generic and informational. The `Error` API accepts only a public message; internal error detail must be routed to `ErrorLog` for server-side logging and never flows into the response body.

### Log Retention Awareness

Logs go to stdout. Operators are responsible for retention policies aligned with GDPR Article 5(1)(e).

## What This Policy Does Not Cover

Security depends as much on what a framework declines to do as on what it enforces. These are out of scope and are your responsibility:

- **Authentication and session management.** RunkoGO ships neither. See the *Deliberately out of scope* table in [CONVENTIONS.md](CONVENTIONS.md#deliberately-out-of-scope).
- **Observability of attacks.** There are no metrics and no tracing spans — [CONV-12](CONVENTIONS.md#conv-12--observability-is-structured-logs-not-metrics). You get structured logs; alerting on them is yours to build.
- **Cluster-wide rate limiting.** [CONV-11](CONVENTIONS.md#conv-11--rate-limiting-is-per-instance-not-per-cluster).
- **Replay safety across services.** The client will not retry a non-idempotent request ([CONV-10](CONVENTIONS.md#conv-10--retries-are-safe-by-default)), but idempotency keys, outboxes and sagas are application concerns.
- **Shutdown under SIGKILL.** If the orchestrator's grace period is shorter than the shutdown budget ([CONV-13](CONVENTIONS.md#conv-13--lifecycle-is-ordered-and-shutdown-has-a-budget)), cleanup is cut short. The framework cannot see that grace period; reconciling the two is an operator task.

## Reporting Vulnerabilities

If you discover a security vulnerability in RunkoGO, please email security@kallioinnovations.fi. Do not open a public issue.

## Supported Versions

Only the latest release receives security updates.
