# RunkoGO Security Policy

RunkoGO is a zero-dependency Go framework where security is a framework-level concern, not an application-level afterthought. This document defines the conventions every RunkoGO application inherits and every contributor must follow.

## Core Principle

**`runko.New()` with zero configuration produces a secure application.** Weakening any security default requires explicit, documented opt-in. Dangerous configurations cause startup panics, not runtime bugs.

## Security Conventions

### CONV-01: Secure by Default, Explicit Opt-In for Trust

The framework never trusts external input without explicit configuration. Forwarding headers (`X-Forwarded-For`) are ignored unless `TrustedProxies` is configured. CORS is denied unless origins are listed. Health check details are hidden unless `HEALTH_DETAIL=true`.

### CONV-02: External Input Sanitization

All values from HTTP headers, query parameters, and request bodies are validated before use in logging, responses, or internal state. Identifiers accept only `[a-zA-Z0-9_-]` up to 64 characters.

### CONV-03: No Internal Details to Unauthenticated Clients

Error responses never contain stack traces, hostnames, connection strings, file paths, or raw library errors. Health checks expose only pass/fail by name, never error messages, unless explicitly enabled.

### CONV-04: Security Headers Available by Default

The framework ships `DefaultSecurityHeaders()` as a ready-to-use middleware. Include it in your middleware chain — both the scaffold and example demonstrate it. Every response then includes `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Cache-Control: no-store`, `Referrer-Policy: strict-origin-when-cross-origin`, and `Permissions-Policy`. HSTS requires explicit opt-in.

### CONV-05: Fail Fast on Dangerous Configuration

CORS wildcard with credentials, invalid trusted proxy entries, and partial TLS configuration all cause startup panics with actionable messages.

### CONV-06: Bounded Resource Consumption

Request bodies: 1MB default via `BodyLimit` middleware. Rate limiter: 10,000 client cap. Service client responses: 10MB cap. Request IDs: 64 character max.

### CONV-07: Timeout Everything

HTTP server has read/write/idle timeouts. Service client has per-request timeout with retries. Health checks have per-check timeouts via context deadlines.

### CONV-08: Audit Trail by Default

Every request is logged by the Logger middleware with method, path, status, duration, request ID, and resolved client IP. Authenticated user identity is logged at the handler level via `LogWithContext()` because auth middleware runs inside the Logger's scope and the enriched context is not available to the outer middleware log line.

## Privacy Conventions

### PRIV-01: No Sensitive Data in Logs

The logger never captures request bodies, response bodies, Authorization header values, or cookie values.

### PRIV-02: No Sensitive Data in URLs

Query strings are not logged by default. When enabled via `LoggerWithConfig`, sensitive parameters (`token`, `key`, `password`, `secret`, `api_key`, `access_token`, `refresh_token`, `session`, `csrf`) are automatically redacted.

### PRIV-03: Request ID ≠ Session Identifier

Request IDs are generated fresh per request, never derived from user identity, and must not be used to track users across requests.

### PRIV-04: Response Headers Reveal Minimum

No `Server` header is set. The framework does not identify itself in responses.

### PRIV-05: Data Minimization in Error Responses

Error responses never echo user input back. Messages are generic and informational.

### PRIV-06: Log Retention Awareness

Logs go to stdout. Operators are responsible for retention policies aligned with GDPR Article 5(1)(e).

## Reporting Vulnerabilities

If you discover a security vulnerability in RunkoGO, please email security@kallioinnovations.fi. Do not open a public issue.

## Supported Versions

Only the latest release receives security updates.
