# Bugs the tests caught

A bug list is evidence that the tests work. An empty one is evidence that they
don't. Every entry names the test or tool that found it, what was actually
wrong, and what changed — including the ones that were my own mistakes in the
test rather than in the product, because confusing those two is itself a common
failure.

Entries are newest last.

---

## 1. Generated `BUILD.bazel` was syntactically invalid

**Found by**: `bazel build //...` on the generated fixture — the first time it
was run, which is the point of running it.

**Symptom**: `indentation error` at line 290, `syntax error at 'outdent'` at the
end of the file.

**Cause**: the generator built each rule with `textwrap.dedent` on an f-string.
`dedent` strips the *common* leading whitespace of a block. The interpolated
dependency list carried its own, shallower indentation, so the common prefix
shrank and every surrounding line came out under-indented. Rules with no
dependencies were fine; rules with dependencies were not, which is why the first
34 targets parsed.

**Fix**: build the rule text by explicit concatenation. Less clever, and it
cannot go wrong this way.

**Falsifier**: the fixture is built end to end in Phase 1 verification; a
malformed `BUILD.bazel` fails immediately and loudly.

---

## 2. The fixture was too small to measure anything

**Found by**: timing the first complete build — 7.4 s wall, 1.09 s critical
path, on 32 cores.

**Symptom**: not a crash. A benchmark that would have "worked" and reported a
meaningless number, which is worse.

**Cause**: 300 targets of trivial C++. Bazel's own loading and analysis
dominated; the compiler barely ran. A cache measured against that workload would
have been measuring Bazel's startup time, and the resulting percentage would
have been real, reproducible, and about the wrong thing.

**Fix**: the generator now emits template-instantiation ballast per translation
unit. The cold build is about 24 s with a 7.7 s critical path, and object files
are a few hundred KiB each, so the benchmark moves a realistic number of bytes
as well as spending realistic CPU.

**Note**: this is the failure mode the repository's first rule is aimed at. The
number would have passed every check except "is this measuring what I claim".

---

## 3. `pkill -f kilncache` killed the shell that ran it

**Found by**: an overnight automation step exiting with status 144 and no output.

**Cause**: `pkill -f` matches against full command lines, and the command line
of the shell running `pkill -f kilncache` contains the string `kilncache`. It
matched itself.

**Fix**: `scripts/localnode.sh`, which tracks PIDs in files. Boring, and cannot
do this.

---

## 4. An integration test raced the server it was testing

**Found by**: `TestClientDisconnectMidUpload`, intermittently — and it was the
*test* that was wrong.

**Symptom**: `partial uploads left in .../tmp: [incoming-2561662586]`.

**Cause**: the test severed a TCP connection mid-body and then immediately
asserted that the temp file was gone. But when a client hangs up, the server
does not find out until its next read fails, which happens after the client has
already moved on. The assertion was racing the cleanup.

**Fix**: two assertions instead of one, with the distinction written down.
`assertNoTempFiles` is used where the server answered *us* — the response cannot
be written until the write path has unwound, so cleanup has provably happened.
`assertNoTempFilesEventually` is used where the client hung up, and waits with a
deadline, because the honest claim there is "cleanup happens promptly", not
"cleanup is faster than the test".

**Why it is recorded**: the tempting fix was a `time.Sleep` before the
assertion, which would have hidden the question rather than answering it.

---

## 5. The container could not create its own data directory

**Found by**: `TestDockerClusterSurvivesNodeLoss`, on its first run —
`container kilncache-node-a is unhealthy`.

**Symptom**:
`open store: storage: create /var/lib/kilncache/tmp: mkdir ...: permission denied`,
in a crash loop.

**Cause**: the runtime image is `distroless/static:nonroot`, so the process runs
as UID 65532. `/var/lib/kilncache` existed only as a volume mount point, not as
a path in the image. When Docker initialises a fresh named volume it copies the
image's contents *and ownership* at that path — and when the path does not exist
in the image, the volume is created owned by root. The nonroot process then
cannot create anything inside it. Distroless has no shell and no `mkdir`, so
there is no startup hook that could have fixed it.

**Fix**: create an empty directory in the build stage and
`COPY --from=build --chown=65532:65532` it to `/var/lib/kilncache`, so the
volume inherits the right ownership at initialisation.

**Why Phase 0 missed it**: Phase 0's compose check started the containers and
curled `/healthz`, but Phase 0 had no storage layer, so nothing ever tried to
write to the volume. The health check passed on a node that could not have
served a single object. This is a good argument for health checks that exercise
the thing being claimed, and `/readyz` now fails until the store opens.
