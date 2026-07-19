# Changelog

## v1.1.0

A correctness and honesty release. Six defects are fixed that let security
controls silently not apply, and every promise the project makes now
resolves to a stated position in [CONVENTIONS.md](CONVENTIONS.md), enforced
by tests that fail the build when code and docs drift apart.

### The module is now installable

`go.mod` declared `github.com/kallioinnovations/runkogo` while the
repository is `github.com/KallioInnovations-Oy/runkogo`. The declared path
404s, and requiring the real path failed on the mismatch:

```
module declares its path as: github.com/kallioinnovations/runkogo
        but was required as: github.com/KallioInnovations-Oy/runkogo
```

So v1.0.0 could not be consumed by anyone, under either path. The module
path now matches the repository:

```go
import runko "github.com/KallioInnovations-Oy/runkogo"
```

This is why the behavior changes below ship as a minor version rather than
a v2: there is no v1 user to break.

### Behavior changes — read before upgrading

No one could install v1.0.0, so nothing here breaks an existing user. They
are listed anyway because they change what a service built on the previous
source tree does, and anyone tracking this repo directly will feel them.

- **Unmatched requests now run the middleware chain.** Previously 404s,
  405s and path-cleaning redirects went straight to `ServeMux` and bypassed
  everything registered with `Use` — no security headers, no request ID, no
  access-log line, no rate limiting. This was the largest hole in the
  framework: an attacker probing for endpoints left no trace at all.
  Root middleware now wraps the router itself.

  *Consequence:* middleware that counts requests now counts unmatched ones
  too. A rate limiter will see 404 traffic it previously ignored.

- **404 and 405 responses are JSON**, not the standard library's
  `text/plain`. Clients parsing those bodies must be updated:

  ```json
  {"error": {"code": "not_found", "message": "Resource not found"}}
  ```

  405 also carries an `Allow` header. Override both with
  `Router.NotFound()` and `Router.MethodNotAllowed()`.

- **Shutdown takes 5 seconds longer by default.** `Options.PreStopDelay`
  (new, default 5s) keeps the listener open after readiness flips to false,
  so a load balancer has time to stop routing. Without it, closing the
  listener immediately produces 502s on every rolling deploy. Set `-1` to
  restore the old behavior.

  Note the budget: `PreStopDelay + ShutdownTimeout + ShutdownTimeout` is 35s
  with defaults, which exceeds Kubernetes' default 30s grace period. See
  [CONV-13](CONVENTIONS.md#conv-13--lifecycle-is-ordered-and-shutdown-has-a-budget).

- **`ClientIP()` can return a different address.** The `X-Forwarded-For`
  walk now terminates at an entry that does not parse as an IP instead of
  skipping it, because everything to the left of a corrupt entry is
  attacker-controlled. All header lines are joined, not just the first.
  Affects rate-limit bucketing, IP allowlists and audit logs.

- **The rate limiter evicts instead of rejecting.** At `MaxClients` it now
  drops the least recently seen bucket rather than refusing unseen clients.
  The old behavior was a denial-of-service: an attacker rotating cheap
  addresses could fill the table and lock out every new legitimate visitor.
  IPv6 is bucketed by /64, so a shared /64 is limited collectively.

- **Group middleware no longer duplicates root middleware.** `Group()`
  carries only the middleware added beyond the root's, since root
  middleware is applied once at dispatch. Previously it was copied into
  each group and could run twice.

- **Minimum Go version is 1.23** (was 1.22), for `http.Request.Pattern`.

### Added

- `Options.ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`, `IdleTimeout`.
  Zero selects the default, negative disables. `WriteTimeout: -1` is what
  makes server-sent events and streaming possible.
- `Options.Handler` plus exported `App.LivenessHandler()` and
  `App.ReadinessHandler()` — use the lifecycle, health checks, security
  middleware and service client with chi or a plain `ServeMux`.
- `Router.Mount(prefix, handler)` for sub-handlers the framework does not
  ship: a metrics endpoint, pprof, an admin UI.
- `Router.NotFound()` and `Router.MethodNotAllowed()`.
- **Error taxonomy**: `AppError` with `NotFound`, `Conflict`, `Validation`,
  `BadRequest`, `Unauthorized`, `Forbidden`, `Internal`, and
  `RespondError`, which finds an `AppError` anywhere in the wrap chain.
  Anything that is not an `AppError` renders as a generic 500, so an
  unvetted driver message cannot reach a client.
- **W3C trace context**: `traceparent` and `tracestate` are validated and
  forwarded byte-for-byte, so a RunkoGO service between two instrumented
  services no longer severs the trace. Read via `runko.Traceparent(ctx)`.
- Access logs include `pattern`, the matched route template, so latency can
  be aggregated by endpoint.
- `CONVENTIONS.md` — the canonical list of every promise, each labelled
  Enforced, Convention, or Deferred, with the enforcing code and defending
  test named. Guard tests fail the build if the docs, the code or the demo
  page drift from it.

### Fixed

- **Circuit breaker could wedge permanently.** A call that ended without an
  upstream verdict — a cancelled context during retry backoff — leaked the
  half-open probe slot, and every subsequent call returned "circuit breaker
  open" for the life of the process.
- **`POST` was retried on transport errors.** A connection reset while
  awaiting a response is not proof the server did not process the request;
  this could double-charge a payment. The idempotency guard now covers
  transport errors as well as 5xx.
- **Retries continued after the breaker opened** mid-call. It is now
  re-checked before each retry.
- **Shutdown hooks were skipped on early returns** from `Run()` — a failed
  listen or an unexpected server error leaked whatever startup acquired.
  They now run exactly once on every path past the first successful startup
  hook.
- **`MaxRetries` with a negative value made zero requests** and returned an
  error wrapping nil. Negative now means one attempt with no retries; zero
  still selects the default of 2.
- Client errors report attempts actually made, not the configured ceiling.
- `PathParam` returned `""` for every route parameter under some routing
  paths.
- `Created()`, CSRF empty-cookie handling, and CORS preflight edge cases.

### Known limitations

Stated rather than implied — see the *Deliberately out of scope* table in
`CONVENTIONS.md`.

- No metrics, tracing spans, or pprof. `Router.Mount` is how you attach
  your own; `traceparent` passes through unchanged.
- `RateLimit` is per-instance. Across N replicas the effective limit is N×.
- CSRF uses an unsigned double-submit cookie, so a compromised sibling
  subdomain can defeat it.
- No authentication, validation, or database layer.

## v1.0.0

Initial release.
