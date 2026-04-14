# RunkoGO Security Policy

RunkoGO is a zero-dependency Go framework where security is a framework-level concern, not an application-level afterthought. This document defines the conventions every RunkoGO application inherits and every contributor must follow.

## Core Principle

**`runko.New()` with zero configuration produces a secure application.** Weakening any security default requires explicit, documented opt-in. Dangerous configurations cause startup panics, not runtime bugs.

## Security Conventions

### Secure by Default, Explicit Opt-In for Trust

The framework never trusts external input without explicit configuration. Forwarding headers (`X-Forwarded-For`) are ignored unless `TrustedProxies` is configured. CORS is denied unless origins are listed. Health check details are hidden unless `HEALTH_DETAIL=true`.

### External Input Sanitization

All values from HTTP headers, query parameters, and request bodies are validated before use in logging, responses, or internal state. Identifiers accept only `[a-zA-Z0-9_-]` up to 64 characters.

### No Internal Details to Unauthenticated Clients

Error responses never contain stack traces, hostnames, connection strings, file paths, or raw library errors. `runko.Error` accepts only a public code and public message; use `runko.ErrorLog` to attach an internal error that goes to the server log with request correlation. Health checks expose only pass/fail by name, never error messages, unless explicitly enabled.

### Security Headers Available by Default

The framework ships `DefaultSecurityHeaders()` as a ready-to-use middleware. Include it in your middleware chain — both the scaffold and example demonstrate it. Every response then includes `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Cache-Control: no-store`, `Referrer-Policy: strict-origin-when-cross-origin`, and `Permissions-Policy`. HSTS requires explicit opt-in.

### TLS Hardening

When TLS is configured, the server enforces a minimum TLS version via `Options.TLSMinVersion`. The default is TLS 1.2 (broad compatibility); new deployments should prefer `tls.VersionTLS13`. Go's default cipher suites apply — they are safe for TLS 1.2+.

### Bounded Request Headers

`http.Server.MaxHeaderBytes` defaults to 64 KiB via `Options.MaxHeaderBytes` — 16× tighter than Go's 1 MB default. This fits typical browser traffic (cookies + JWT + tracing headers) while rejecting abusive payloads. Raise for SSO-heavy deployments that carry large SAML cookies.

### CSRF Protection for Cookie-Authenticated Apps

The `CSRF` middleware implements the double-submit-cookie pattern for webapps that rely on cookie authentication. It issues a token cookie on safe methods and requires the value to be echoed in the `X-CSRF-Token` request header on unsafe methods. For purely token-authenticated APIs (Authorization: Bearer …), enable `SkipAuthHeader` to bypass the check — bearer tokens are not sent by browsers automatically.

**Known limitation — subdomain cookie injection.** The middleware stores the CSRF token as a plain cookie (not HMAC-signed against a session). An attacker who controls a sibling subdomain (via XSS on `evil.example.com` or a compromised host) can set the parent-domain cookie via `document.cookie` and defeat the check. The standard mitigation is a signed double-submit cookie tied to the session identifier; RunkoGO does not yet ship session infrastructure, so the signed variant is unavailable. Until it is, compensate at the infrastructure layer: restrict which subdomains can be provisioned, enable HSTS with `includeSubDomains`, and prefer `SameSite=Strict` where the application allows.

### Fail Fast on Dangerous Configuration

CORS wildcard with credentials, invalid trusted proxy entries, and partial TLS configuration all cause startup panics with actionable messages.

### Bounded Resource Consumption

Request bodies: 1 MB default via `BodyLimit`. Request headers: 64 KiB default. Rate limiter: 10 000 client cap. Service client responses: 10 MB cap. Request IDs: 64 character max.

### Timeout Everything

HTTP server has read/write/idle timeouts. Service client has per-request timeout with retries. Health checks have per-check timeouts via context deadlines.

### Audit Trail by Default

Every request is logged by the Logger middleware with method, path, status, duration, request ID, and resolved client IP. Authenticated user identity is logged at the handler level via `LogWithContext()` because auth middleware runs inside the Logger's scope and the enriched context is not available to the outer middleware log line.

## Privacy Conventions

### No Sensitive Data in Logs

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

## Reporting Vulnerabilities

If you discover a security vulnerability in RunkoGO, please email security@kallioinnovations.fi. Do not open a public issue.

## Supported Versions

Only the latest release receives security updates.
