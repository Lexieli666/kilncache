# The Bazel HTTP remote cache protocol, as KilnCache implements it

Bazel's HTTP remote cache is not a formal specification. It is whatever
`--remote_cache=http://host:port` does, which is a deliberately small REST
surface. This document records exactly what KilnCache assumes, so that a future
change to Bazel that breaks one of these assumptions has something to be checked
against.

## Endpoints

| Method | Path | Meaning |
|---|---|---|
| `GET` | `/cas/<sha256>` | Fetch a content-addressed blob. |
| `HEAD` | `/cas/<sha256>` | Existence and size, no body. |
| `PUT` | `/cas/<sha256>` | Store a blob whose SHA-256 is the key. |
| `GET` | `/ac/<sha256>` | Fetch a serialized `ActionResult`. |
| `HEAD` | `/ac/<sha256>` | Existence and size, no body. |
| `PUT` | `/ac/<sha256>` | Store a serialized `ActionResult`. |

`POST` is accepted as a synonym for `PUT`, because some clients use it.

A `/cache/` prefix is also accepted (`/cache/cas/<sha256>`), for deployments
configured behind a path-prefixed reverse proxy. Both spellings address the same
objects; they are not separate namespaces.

## Status codes and what they mean to a build

This matters more than it looks. **Bazel treats a non-404 error from the cache
as a reason to disable the remote cache for the remainder of the invocation.** A
single spurious 500 does not cost one object — it costs the whole build its
cache. Every status here is chosen with that in mind.

| Status | When | Why not something else |
|---|---|---|
| `200` | GET/HEAD hit; PUT of an object already stored | — |
| `201` | PUT stored a new object | Informative; Bazel accepts any 2xx. |
| `404` | GET/HEAD miss, **and any malformed key or unknown namespace on a read** | A garbled key is a miss. Returning 400 here would disable the cache for the build over one bad request. |
| `400` | PUT with a digest mismatch, a body that does not match Content-Length, or a malformed key | The client has a bug; retrying unchanged cannot help, so it should be told. |
| `405` | Any other method | With an `Allow` header. |
| `413` | Object exceeds the configured per-object limit | Sent from the declared Content-Length before a byte is read, when possible. |
| `499` | The client disconnected mid-upload | nginx's convention. Nothing was published. Appears only in this node's own logs and metrics — by the time it is written the client is gone — and exists so an abandoned upload is visibly not a server error. |
| `503` | The node is shutting down, or (from Phase 2) the required number of replicas could not be written | An honest refusal. A PUT that returns 200 without the second copy would make the replication factor a fiction. |
| `500` | Anything unexpected | Logged with the key. |

## Headers

Sent on every response:

- `X-Kilncache-Node`: the node that produced the response. Integration tests use
  it to assert that a read served through node B really came from node A's copy.

Sent where relevant:

- `X-Kilncache-Source`: `local`, `primary`, or `replica` — which copy answered.
- `X-Kilncache-Forwarded-By`: set on an internal, node-to-node request. A node
  that receives a request carrying this header serves it locally or fails; it
  never forwards again. That is the entire loop-prevention mechanism.
- `X-Kilncache-Already-Stored`: `true` when a PUT was a no-op.

`Cache-Control: no-store` is set on object responses. CAS objects are immutable
and AC entries are explicitly overwritten; in neither case should an
intermediary serve a stale copy.

## Assumptions, and what breaks if they are wrong

1. **Keys are exactly 64 lowercase hex characters.** Bazel sends lowercase hex
   SHA-256. Uppercase is *rejected* rather than normalised: accepting both and
   normalising would collide two keys Bazel considers distinct, and accepting
   both without normalising would store one object twice under names that differ
   only in case — which behaves differently on a case-sensitive and a
   case-insensitive filesystem. Falsifier: `TestValidateKeyRejects`.

2. **A CAS key is the SHA-256 of the bytes.** This is the foundation of
   ADR-0002. Uploads are verified and rejected on mismatch. If Bazel ever sent a
   CAS key that was not the content hash, every PUT would 400 and the failure
   would be immediate and obvious rather than silent.

3. **An AC entry's key is a hash of the action, not of the value.** The server
   does not parse or verify AC bodies and permits overwrite. This is the one
   place the design is weaker than a consensus-based cache; ADR-0002 and
   `docs/failure-model.md` say so explicitly.

4. **Content-Length is present on uploads, or the body is chunked.** When it is
   present it is enforced. When it is absent (`-1`), the object is accepted at
   whatever length arrives — for CAS the digest check still catches truncation;
   for AC there is nothing to check against, which is stated rather than hidden.

5. **Range requests are not used.** Bazel fetches whole blobs. KilnCache does
   not advertise `Accept-Ranges` and does not implement `Range`.

6. **No authentication.** `--remote_cache` supports headers and TLS; KilnCache
   implements neither. The trust boundary is the cluster. This is in the
   README's non-goals, not an oversight.

7. **Compression is not negotiated.** Build artifacts are usually already
   compressed, and re-compressing them would spend CPU on the hot path for
   little gain. Bodies are transferred as stored.

## What Bazel does with this

```bash
bazel build //... --remote_cache=http://localhost:8080
```

Roughly: for each action, Bazel computes an action digest, does
`GET /ac/<action-digest>`. On a hit it reads the `ActionResult`, which names the
output blobs by digest, and fetches each with `GET /cas/<digest>`. On a miss it
executes the action locally, `PUT`s each output to `/cas/<digest>`, then `PUT`s
the `ActionResult` to `/ac/<action-digest>`.

Useful flags when working on this:

- `--remote_upload_local_results=true` (default) — without it the cache is
  read-only and never populates.
- `--experimental_remote_cache_async=false` — makes upload timing deterministic,
  which matters when measuring.
- `--remote_timeout=60s` — the default is short enough that a slow first
  population run can look like a cache failure.
- `--execution_log_binary_file=` / `--build_event_json_file=` — the source of
  the hit/miss counts the Phase 5 benchmark records.
