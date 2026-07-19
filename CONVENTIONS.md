# RunkoGO Conventions

This is the canonical list of every promise RunkoGO makes. It is the source
of truth: [SECURITY.md](SECURITY.md), [README.md](README.md), the scaffold's
demo page, and the doc comments in the source all reference these IDs rather
than restating the promises in their own words.

RunkoGO is a starting point, not a batteries-included framework. It does not
ship everything a production service needs. What it does promise is that
every claim it makes resolves to one of exactly three states — nothing
floats:

| State | Meaning | What backs it |
|---|---|---|
| **Enforced** | The framework guarantees it. | Code, plus a test that fails if the guarantee breaks. |
| **Convention** | You must do something for it to hold. | Documented here, demonstrated in `scaffold/`. The framework cannot enforce it for you. |
| **Deferred** | Deliberately out of scope. | Named here with the reason and the recommended alternative. |

A claim that is none of these is a bug in the documentation. `TestConventions_*`
in [conventions_test.go](conventions_test.go) fails the build if any `CONV-xx`
referenced anywhere in the repo is missing from this file.

**Reading the entries:** *Enforced by* cites the code that implements the
convention; *Defended by* cites the test that fails if it regresses. Where a
convention has real limits, they are stated under *Limits* — a documented
limit is part of the promise, not a footnote to it.

---

## CONV-01 — Secure by default, explicit opt-in for trust

**State: Enforced**

The framework never trusts external input without explicit configuration.

- `X-Forwarded-For` is ignored entirely unless `Options.TrustedProxies` is
  set. With no proxies configured, `RemoteAddr` is always used.
- CORS denies every origin unless it is listed. There is no reflect-any mode.
- `/readyz` reports failing check *names* only; error text requires
  `HEALTH_DETAIL=true`.

When trusted proxies *are* configured, the `X-Forwarded-For` chain is walked
right-to-left and the first untrusted entry is the client. All header lines
are joined first, because some proxies append a new line rather than
extending the existing one — reading only the first would let a client
inject its own address ahead of the proxy's.

**Enforced by:** [proxy.go](proxy.go) (`resolveClientIP`), [middleware.go](middleware.go) (`ClientIPMiddleware`, `CORS`), [app.go](app.go) (`readinessHandler`)
**Defended by:** `TestClientIPMiddleware_IgnoresXFFWithoutTrustedProxies`, `TestClientIPMiddleware_MultipleXFFHeaderLines`, `TestProxyResolver_InvalidIPInXFF_TerminatesWalk`, `TestCORS_DisallowedOrigin`, `TestApp_ReadinessHandler_FailingCheck`

**Limits:**
- An `X-Forwarded-For` entry that does not parse as an IP terminates the
  walk and falls back to `RemoteAddr`. Everything to the left of a corrupt
  entry is attacker-reachable, so it cannot be trusted. The `ip:port` form
  is normalized first, since that is a legitimate chain, not corruption.
- **`X-Real-IP` is not read.** If your proxy sets only `X-Real-IP` (a common
  nginx recipe), configure it to set `X-Forwarded-For` as well, or every
  request will resolve to the proxy's own address.

---

## CONV-02 — External input is sanitized before use

**State: Enforced**

Identifiers arriving from clients are validated before they reach logs,
responses, or internal state. `X-Request-ID` and `X-Trace-ID` accept only
`[a-zA-Z0-9_-]`, maximum 64 characters. An invalid `X-Request-ID` is
replaced by a freshly generated one; an invalid `X-Trace-ID` is dropped
entirely, since a trace ID is only meaningful if it came from upstream. This blocks log injection — newlines and JSON
fragments crafted to corrupt structured log output.

Request bodies are decoded with unknown-field rejection and a trailing-data
check, so a body is either exactly what the handler expects or an error.

**Enforced by:** [sanitize.go](sanitize.go) (`sanitizeID`), [context.go](context.go), [response.go](response.go) (`DecodeWithLimit`)
**Defended by:** `TestSanitizeID`, `TestRequestIDMiddleware_SanitizesInput`, `TestRequestIDMiddleware_InvalidTraceIDDropped`, `TestDecode_UnknownFields_Rejected`, `TestDecode_TrailingData_Rejected`

---

## CONV-03 — No internal detail reaches an unauthenticated client

**State: Enforced (API shape) + Convention (message content)**

`runko.Error(w, status, code, publicMsg)` has no parameter capable of
carrying an internal error, so stack traces, connection strings and file
paths cannot leak through it by accident. To record the underlying cause,
use `runko.ErrorLog(w, r, logger, status, code, publicMsg, err)`, which
sends it to the server log correlated by request ID.

**Your part:** the framework cannot inspect the string you pass as
`publicMsg`. Do not interpolate error text, hostnames, or query fragments
into it. Prefer `ErrorLog` over `Error` whenever you have an `err` in hand —
a 500 with no server-side record is an outage you cannot debug.

**Enforced by:** [response.go](response.go) (`Error`, `ErrorLog`), [app.go](app.go) (`readinessHandler`)
**Defended by:** `TestError_Response`, `TestApp_ReadinessHandler_FailingCheck`, `TestApp_ReadinessHandler_DetailMode`

---

## CONV-04 — Security headers on every response

**State: Convention (register it) + Enforced (coverage, once registered)**

`DefaultSecurityHeaders()` is middleware, not automatic behavior. **You must
add it to the root middleware chain.** Once it is there, the framework
guarantees it reaches *every* response: matched routes, 404s, 405s, and
path-cleaning redirects alike.

That coverage guarantee is load-bearing and was not always true. Root
middleware wraps the mux itself rather than being frozen onto individual
routes, precisely so that responses which never reach a route cannot escape
it. Registering security headers per-route, or on a `Group`, leaves 404s
uncovered.

Headers set: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`X-XSS-Protection: 0`, `Referrer-Policy: strict-origin-when-cross-origin`,
`Cache-Control: no-store`, `Permissions-Policy`. HSTS and CSP require
explicit opt-in. No `Server` header is ever set.

**Enforced by:** [security.go](security.go) (`SecurityHeaders`), [router.go](router.go) (`ServeHTTP`, `dispatch`)
**Defended by:** `TestSecurityHeaders_Defaults`, `TestUnmatchedRequestsRunMiddlewareChain`, `TestRedirectsStillWork`

**Limits:** HSTS always includes `includeSubDomains`, which commits every
sibling subdomain to HTTPS for the max-age. Enable it only when you control
all of them.

**Ordering matters.** Middleware that answers a request itself — `CORS`
preflight, `AllowedHosts` rejection, `RateLimit` 429, `CSRF` rejection —
returns without calling the rest of the chain. Register those *after*
`Logger` and `DefaultSecurityHeaders`, or the responses they generate skip
both. The framework guarantees the chain runs for every request; it cannot
guarantee your ordering within it.

---

## CONV-05 — Dangerous configuration fails at startup, not at runtime

**State: Enforced**

Configurations that would silently weaken security panic during
construction, with a message naming the fix:

- CORS `AllowedOrigins: ["*"]` combined with `AllowCredentials: true`
- A `TrustedProxies` entry that is not a valid IP or CIDR
- `TLSCert` set without `TLSKey`, or vice versa
- `AllowedHosts` with an empty host list

These are startup panics by design. A misconfigured security control that
starts successfully is worse than a process that refuses to boot.

**Enforced by:** [middleware.go](middleware.go) (`CORS`), [proxy.go](proxy.go) (`newProxyResolver`), [app.go](app.go) (`New`), [hosts.go](hosts.go) (`AllowedHosts`)
**Defended by:** `TestCORS_WildcardWithCredentials_Panics`, `TestProxyResolver_InvalidEntry_Panics`, `TestApp_New_PartialTLS_Panics`, `TestAllowedHosts_EmptyConfig_Panics`

---

## CONV-06 — Resource consumption is bounded

**State: Enforced (framework-level) + Convention (per-route limits)**

Every input path has a ceiling:

| Resource | Bound | Applied |
|---|---|---|
| Request headers | 64 KiB | Automatic (`Options.MaxHeaderBytes`) |
| Request body | 1 MiB | **You must register `BodyLimit`** |
| Rate limiter table | 10 000 buckets | Automatic once `RateLimit` is registered |
| Service client response | 10 MiB | Automatic (`MaxResponseSize`) |
| Request / trace IDs | 64 chars | Automatic |

**Rate limiter table:** at capacity the **least recently seen bucket is
evicted**. It never rejects an unseen client. Rejecting at capacity would
let an attacker rotating cheap addresses fill the table and deny service to
every new legitimate client — a more effective attack than the flooding the
limiter exists to stop.

**IPv6 is bucketed by /64,** not by full address. A single IPv6 client is
routinely delegated 2^64 addresses; keying on the full address would make
the limiter a no-op. Note the consequence: a shared /64 — a campus, a
mobile carrier block — is limited collectively.

**Enforced by:** [middleware.go](middleware.go) (`rateLimiter`, `rateLimitKey`, `BodyLimit`), [client.go](client.go) (`newLimitedReadCloser`), [app.go](app.go)
**Defended by:** `TestRateLimit_TableStaysBounded`, `TestRateLimit_AtCapacityEvictsLRUAndAdmitsNewClients`, `TestRateLimit_IPv6BucketedBySlash64`, `TestRateLimit_CleansUpExpiredEntries`, `TestBodyLimit_Enforced`, `TestDo_ResponseBodyLimited`

**Limits:** see [CONV-11](#conv-11--rate-limiting-is-per-instance-not-per-cluster).

---

## CONV-07 — Everything has a timeout

**State: Enforced**

No operation waits indefinitely. The HTTP server sets read, read-header,
write and idle timeouts; the service client applies a per-request timeout
with bounded retries; every health check runs under its own context
deadline with panic recovery, so one bad check cannot stall the readiness
report.

**Enforced by:** [app.go](app.go) (`Run`, `runHealthCheck`), [client.go](client.go)
**Defended by:** `TestApp_ServerTimeoutsAreSet`, `TestApp_TimeoutOptions`, `TestApp_StreamingWorksWithoutWriteTimeout`, `TestApp_ReadinessHandler_CheckTimeout`

All four server timeouts are configurable through `Options`, defaulting to
`ReadTimeout` 30s, `ReadHeaderTimeout` 10s, `WriteTimeout` 30s,
`IdleTimeout` 120s. Zero selects the default; a negative value disables that
timeout, since `http.Server` spells "no timeout" as zero and the zero value
is already taken.

Streaming needs the write deadline gone, and the middleware already
preserves `http.Flusher` and `http.Hijacker` for exactly that:

```go
app := runko.New(runko.Options{WriteTimeout: -1}) // SSE, long-polling
```

**Limits:** disabling a timeout means a stalled client holds the connection
until `IdleTimeout` or the OS gives up, so prefer a generous value over
`-1` unless the response really is unbounded. `ReadTimeout` bounds the whole
request including the body, so raise it for large uploads rather than
relying on the body limit alone.

---

## CONV-08 — Audit trail by default

**State: Convention (register it) + Enforced (coverage, once registered)**

`Logger(app.Logger)` is middleware — **you must register it.** Once
registered, every request produces one structured line: method, path,
status, duration, request ID and resolved client IP. That includes requests
that match no route, which is where unlogged traffic used to hide: an
attacker probing for endpoints generated no record at all.

Authenticated user identity is logged at the handler level via
`LogWithContext()`, not by this middleware — auth runs *inside* the logger's
scope, so the enriched context does not exist yet when the outer log line is
written.

**Enforced by:** [middleware.go](middleware.go) (`LoggerWithConfig`), [router.go](router.go) (`ServeHTTP`)
**Defended by:** `TestUnmatchedRequestsRunMiddlewareChain`, `TestRedirectsStillWork`, `TestMatchedRouteRunsMiddlewareExactlyOnce`

**Limits:** logs record the matched route template as `pattern` alongside
the concrete `path`, so latency can be aggregated by endpoint. The
attribute is omitted for unmatched requests, and — importantly — if
`Logger` is registered *before* a middleware that clones the request
(`RequestIDMiddleware` and `ClientIPMiddleware` both do, via
`r.WithContext`). The mux stamps the clone, which `Logger` no longer holds.
Register `Logger` after them, as the example and scaffold do.

**Ordering matters.** Middleware that answers a request itself — `CORS`
preflight, `AllowedHosts` rejection, `RateLimit` 429, `CSRF` rejection —
returns without calling the rest of the chain. Register those *after*
`Logger` and `DefaultSecurityHeaders`, or the responses they generate skip
both. The framework guarantees the chain runs for every request; it cannot
guarantee your ordering within it.

---

## CONV-09 — Privacy: sensitive data stays out of logs and URLs

**State: Enforced**

The logger never captures request bodies, response bodies, `Authorization`
header values, or cookie values. Query strings are not logged at all by
default; enabling them via `LoggerWithConfig` redacts known-sensitive
parameters (`token`, `key`, `password`, `secret`, `api_key`, `access_token`,
`refresh_token`, `session`, `csrf`, and similar).

Request IDs are correlation aids, never security tokens: generated fresh per
request, never derived from user identity, and not to be used to track users
across requests.

**Enforced by:** [middleware.go](middleware.go) (`redactQuery`, `sensitiveParams`), [context.go](context.go)
**Defended by:** `TestRedactQuery`, `TestRequestIDMiddleware_GeneratesID`

**Your part:** logs go to stdout. Retention, and its alignment with GDPR
Article 5(1)(e), is the operator's responsibility.

---

## CONV-10 — Retries are safe by default

**State: Enforced**

The service client never replays a non-idempotent request. `POST` is not
retried on 5xx responses *or* on transport errors unless you set
`RetryNonIdempotent: true` — which you should only do when your
endpoints use idempotency keys.

A transport error is deliberately treated the same as a 5xx here. A
connection reset while awaiting a response is not proof the server did not
process the request; retrying can double-charge a payment.

The circuit breaker is re-checked before every retry, so one call cannot
keep hammering a service the breaker has already decided to shed. A call
that ends without an upstream verdict — a cancelled context, an unmarshalable
body — releases the breaker's probe slot and restarts the cooldown rather
than leaving it wedged.

**Enforced by:** [client.go](client.go) (`do`, `circuitBreaker`, `isIdempotent`)
**Defended by:** `TestServiceClient_TransportErrorDoesNotRetryPost`, `TestServiceClient_TransportErrorRetriesGet`, `TestServiceClient_NegativeMaxRetriesDisablesRetries`, `TestServiceClient_ZeroMaxRetriesUsesDefault`, `TestServiceClient_ErrorReportsActualAttempts`, `TestServiceClient_StopsRetryingOnceBreakerOpens`, `TestServiceClient_ContextCancellationDoesNotWedgeBreaker`, `TestCircuitBreaker_AbortReleasesHalfOpenProbe`, `TestCircuitBreaker_AbortRestartsCooldown`, `TestDo_POST_NoRetryByDefault`

**Limits:** `MaxRetries` is a ceiling, not a guarantee — the breaker can cut
retries short, and the error reports the attempts actually made rather than
the configured maximum. Zero means "use the default of 2"; pass a negative
value for a single attempt with no retries. The breaker is per-client-instance and per-process; it is not
shared across replicas. `ServiceClient` exposes `Get`, `Post`, `Put`,
`Delete` and `GetJSON`; there is no `Patch` method, so the idempotency rule
covers `POST` in practice.

---

## CONV-11 — Rate limiting is per-instance, not per-cluster

**State: Convention**

`RateLimit` runs in-process with no external state. In a deployment of N
replicas, a configured limit of 100 req/min is effectively **100 × N**, and
every counter resets on deploy.

**Treat it as a last-resort backstop against a single abusive client
exhausting one instance — not as your cluster's rate limit.** Cluster-wide
limiting belongs at the ingress or API gateway, where request counts are
already aggregated. If you need a correct distributed limit, use a shared
store; RunkoGO deliberately ships no such dependency.

**Enforced by:** nothing — this is a property of the design, stated so it is
not mistaken for a guarantee.
**Related:** [CONV-06](#conv-06--resource-consumption-is-bounded)

---

## CONV-12 — Observability is structured logs, not metrics

**State: Deferred (metrics and spans) + Enforced (Mount, traceparent pass-through)**

RunkoGO emits structured JSON logs via `slog`. It ships **no metrics, no
tracing spans, and no profiling endpoint** — there is no `/metrics`, no
`expvar`, no OpenTelemetry SDK, no `/debug/pprof`.

This is a scope decision, not an oversight: metrics and tracing are where a
zero-dependency constraint stops paying for itself, and every team already
has an opinion about the collector.

**What the framework does do**, so that bringing your own is possible
rather than obstructed:

- **`Router.Mount(prefix, handler)`** attaches any `http.Handler` at a path
  prefix, for every method and the whole subtree beneath it. This is how a
  collector endpoint or pprof gets attached. Mounted handlers run inside
  the root middleware chain; mount them on a `Group` to put them behind
  auth, which pprof in particular warrants.

  ```go
  app.Router.Mount("/metrics", promhttp.Handler())
  admin := app.Router.Group("/debug", requireAdmin)
  admin.Mount("/pprof", http.DefaultServeMux)
  ```

- **W3C `traceparent` and `tracestate` are propagated unchanged.** Both are
  validated, carried on the request context, and forwarded byte-for-byte by
  `ServiceClient`. RunkoGO creates no spans and never synthesizes or mutates
  either value — it simply refuses to be the hop that severs a trace passing
  through it. Read them with `runko.Traceparent(ctx)` and
  `runko.Tracestate(ctx)`.

  Higher `traceparent` versions are forwarded rather than dropped, because
  §3.2.4 makes the format additive and rejecting them would sever a trace
  the moment an upstream adopts version `01`. A request carrying more than
  one `traceparent` is ambiguous and is discarded rather than resolved by
  taking the first, which a client could otherwise prepend.

**What is still yours on day one:** the metrics themselves. There are no
counters or histograms; `Logger` computes status and duration per request
and puts them in a log line rather than a metric. If you want RED metrics,
wrap a middleware of your own around the chain and expose it via `Mount`.

**Enforced by:** [router.go](router.go) (`Mount`), [context.go](context.go) (`Traceparent`), [sanitize.go](sanitize.go) (`sanitizeTraceparent`), [client.go](client.go)
**Defended by:** `TestMountServesPrefixAndSubtree`, `TestMountRunsRootMiddleware`, `TestMountOnGroupInheritsGroupMiddleware`, `TestMountRejectsUncleanCombinedPattern`, `TestSanitizeTraceparent`, `TestSanitizeTracestate`, `TestTraceparentPropagatesEndToEnd`, `TestTracestatePropagatesWithTraceparent`, `TestTraceparentFromHeader_RejectsAmbiguousDuplicates`

**Limits:** version `ff` is rejected as the spec requires, and a
`tracestate` longer than the spec's recommended 512 characters is dropped
rather than truncated. RunkoGO does not downgrade a higher version to `00`
on the way out: that rule binds implementations which make themselves the
parent span, and a pure conduit passing the original through preserves
information a version-aware downstream can still use. See also
[CONV-08](#conv-08--audit-trail-by-default) limits.

---

## CONV-13 — Lifecycle is ordered, and shutdown has a budget

**State: Enforced (ordering) + Convention (the budget)**

Startup runs hooks in registration order and aborts on the first error —
the server never starts with a half-initialised dependency. Shutdown runs in
a fixed order:

1. Readiness flips to **false** — `/readyz` starts failing immediately.
2. The server keeps serving for `PreStopDelay` **with the listener open**.
3. In-flight requests drain (`ShutdownTimeout`).
4. Shutdown hooks run, with a fresh full `ShutdownTimeout`.

Step 2 exists because orchestrators remove a pod from the load balancer
asynchronously. Closing the listener the instant readiness flips means the
balancer is still routing to a socket that now refuses connections — 502s on
every rolling deploy.

**Shutdown hooks run on early returns too:** a failed `net.Listen`, an
unexpected server error, or a startup hook that fails *after* an earlier one
succeeded. They are skipped only when the *first* startup hook fails, since
nothing was acquired and a hook may reasonably assume startup completed.
They run exactly once in all cases.

**Your part — the shutdown budget.** The three phases are sequential, so the
worst case is their sum:

```
PreStopDelay + ShutdownTimeout (drain) + ShutdownTimeout (hooks)
       5s     +        15s             +        15s              = 35s
```

Kubernetes defaults `terminationGracePeriodSeconds` to **30**, so with stock
settings a worst-case shutdown is SIGKILLed partway through the shutdown
hooks — losing exactly the cleanup they exist to perform. Reconcile the two:
either raise the grace period above the sum, or lower the sum beneath it.

The framework does not clamp these values. It cannot see the grace period,
and silently shortening a drain you asked for would cut off in-flight
requests.

**Enforced by:** [app.go](app.go) (`Run`)
**Defended by:** `TestApp_RunLifecycle_Full`, `TestApp_PreStopDelayKeepsServingAfterUnready`, `TestApp_ShutdownHooksRunWhenListenFails`, `TestApp_ShutdownHooksRunWhenLaterStartupHookFails`, `TestApp_ShutdownHooksSkippedWhenFirstStartupHookFails`, `TestApp_ShutdownHooksRunExactlyOnce`, `TestApp_ShutdownHooksGetFreshTimeout`

---

## CONV-14 — CSRF protection for cookie-authenticated apps

**State: Convention**

`CSRF` implements the double-submit-cookie pattern: a token cookie is issued
on safe methods and must be echoed in the `X-CSRF-Token` header on unsafe
ones. Tokens are 32 bytes from `crypto/rand` and compared in constant time.

**You must register it**, and only for cookie-authenticated routes. For
purely bearer-token APIs set `SkipAuthHeader` — browsers do not attach
`Authorization` automatically, so those endpoints are not CSRF-reachable.

**Enforced by:** [csrf.go](csrf.go)
**Defended by:** `TestCSRF_AcceptsMatchedToken`, `TestCSRF_RejectsMismatchedToken`, `TestCSRF_RejectsUnsafeWithoutToken`, `TestCSRF_EmptyCookieRejected`, `TestCSRF_SkipAuthHeaderBypasses`

**Demonstrated in `scaffold/`** at `/app/profile`: the GET issues the token
cookie, the POST requires it echoed in `X-CSRF-Token`. The demo page
exercises both, including the rejection path, so the convention can be
checked rather than taken on trust.

**Limits — subdomain cookie injection.** The token is a plain cookie, not
HMAC-signed against a session. An attacker controlling a sibling subdomain
can set the parent-domain cookie and defeat the check. RunkoGO ships no
session infrastructure, so the signed variant is unavailable. Compensate at
the infrastructure layer: restrict subdomain provisioning, enable HSTS with
`includeSubDomains`, and prefer `SameSite=Strict` where the app allows.

---

## CONV-15 — Errors carry their own disclosure decision

**State: Convention**

`AppError` binds an HTTP status, a public code, a public message and an
internal cause into one value, so the decision about what a client may see
is made where the error is created rather than re-derived in every handler.

```go
// In the store, where the failure is understood:
return runko.NotFound("User not found").Wrap(err)

// In every handler, unconditionally:
if err != nil {
    runko.RespondError(w, r, app.Logger, err)
    return
}
```

`RespondError` finds an `AppError` anywhere in the wrap chain via
`errors.As`, so a sentinel created three layers down still renders
correctly at the boundary. Helpers cover the common statuses:
`BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `Conflict`,
`Validation`, `Internal`.

**An error that is not an `AppError` renders as a generic 500.** This is the
load-bearing rule: an error that never passed through this vocabulary has
not been vetted for disclosure, so a raw driver message naming a host and a
username cannot reach a client by default. The cause is logged with request
correlation; 5xx logs at error level and 4xx at warn, because a client
mistake is not a server fault and should not page anyone.

**Your part:** `Message` and `Details` are rendered verbatim. The type
routes internal detail to the log for you, but it cannot tell whether the
string you passed as the public message is safe — that remains
[CONV-03](#conv-03--no-internal-detail-reaches-an-unauthenticated-client).

**Enforced by:** [errors.go](errors.go) (`AppError`, `RespondError`)
**Defended by:** `TestRespondError_UnknownErrorIsGeneric500`, `TestRespondError_CauseIsLoggedNotRendered`, `TestRespondError_FindsAppErrorThroughWrapping`, `TestRespondError_LogLevelBySeverity`, `TestRespondError_TypedNilAppErrorDoesNotPanic`, `TestAppError_UnwrapsToCause`, `TestAppError_DetailsAreNotAliasedAcrossCopies`

**Limits:** `RespondError` writes the response itself, so it must be called
before anything else writes. There is no automatic mapping from third-party
error types — wrap them at the boundary where you understand them. `Details`
is copied one level deep, so do not mutate values stored inside it. Set
`Cause` with `Wrap` rather than by assignment: an AppError reachable from
its own `Cause` makes `Error()` recurse until the process dies.

---

## Deliberately out of scope

Named so you can plan for them rather than discover them. None of these are
on a roadmap; the recommendation is what to reach for instead.

| Not shipped | Use instead |
|---|---|
| Authentication (JWT, sessions, OAuth) | Your identity provider's Go SDK; see `scaffold/` for the middleware shape |
| Request validation | `go-playground/validator`, or hand-rolled — `Decode` gives you a typed struct |
| Metrics, tracing, profiling | See [CONV-12](#conv-12--observability-is-structured-logs-not-metrics) |
| Distributed rate limiting | Ingress/gateway; see [CONV-11](#conv-11--rate-limiting-is-per-instance-not-per-cluster) |
| Database layer, migrations | `database/sql`, `pgx`, `goose`; see `scaffold/store.go` for the interface boundary |
| OpenAPI / schema generation | `swaggo`, or write the spec by hand |
| Compression, ETag, content negotiation | `net/http` middleware from the ecosystem — handlers are plain `http.HandlerFunc`, so anything composes |
| Service discovery, client-side load balancing | Your orchestrator's DNS; `ServiceClient` targets one `BaseURL` |
| Background jobs, cron, queues | A dedicated worker binary; `App` lifecycle hooks manage its startup and shutdown |
| SSE / streaming beyond 30s | Not currently possible — see [CONV-07](#conv-07--everything-has-a-timeout) limits |

**Why handlers are plain `http.HandlerFunc`:** every item above stays
solvable with off-the-shelf code. There is no custom context type and no
framework-specific handler signature, so any `net/http` middleware — from
the standard library, `chi`, or anywhere else — composes without an adapter.
That is the deliberate escape hatch for everything RunkoGO does not do.
