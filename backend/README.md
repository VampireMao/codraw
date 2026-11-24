# CoDraw backend

Go 1.27 server for 同绘 CoDraw: REST API, WebSocket collaboration hub, embedded
Vue SPA host, Prometheus metrics and an operator panel.

The contract is [`docs/PROTOCOL.md`](../docs/PROTOCOL.md). When code and that
document disagree, the document wins.

## Quick start

```bash
cd backend
go run ./cmd/server          # :8080, in-memory store + broker, no external services
```

| URL | What it is |
|---|---|
| `POST /api/rooms` | create a room, returns the keys **once** |
| `GET /api/rooms/{code}` | public metadata |
| `POST /api/rooms/{code}/readonly-token` | mint a signed viewer link |
| `GET /api/rooms/{code}/snapshot` | paged snapshot fallback |
| `GET /ws` | WebSocket upgrade |
| `GET /healthz` `/readyz` | liveness / readiness |
| `GET /metrics` `/ops` `/ops/data` | Prometheus / operator panel |
| `GET /` | the SPA, or a "frontend not built" page |

No configuration is required. `.env` files are **not** loaded; configuration comes
from the environment so containers behave identically to a shell. Every variable
is documented in [`.env.example`](.env.example).

## Verifying

```bash
../scripts/test-backend.sh              # gofmt, tidy, vet, race tests, contract, e2e
../scripts/test-backend.sh --two-inst   # + Redis/Postgres two-instance check
../scripts/test-backend.sh --docker     # + image build and size
```

Individual pieces:

| Script | What it proves |
|---|---|
| `scripts/check-contract.sh` | the 12 contract-alignment items (envelope, types, alphabet, …) |
| `scripts/e2e-memory.sh` | 11 end-to-end assertion groups against the real binary |
| `scripts/check-two-instances.sh` | globally monotonic seq + convergence across two instances |
| `scripts/sync-webui.sh` | copies `web/dist` into the embed dir, no-ops when absent |
| `scripts/report-tests.sh` | prints vet/test output for a review |

`cmd/wssmoke` drives a real WebSocket session and prints every frame:

```bash
go run ./cmd/wssmoke -base http://localhost:8080 -create
go run ./cmd/wssmoke -room K7M2QX -key <editorKey> -send-op 2 -same-op-id
go run ./cmd/wssmoke -room K7M2QX -key <viewerKey> -send-op 1   # expect forbidden
```

## Layout

```
cmd/server              composition root, graceful shutdown
cmd/wssmoke             protocol smoke client
internal/protocol       envelope, messages, shape model, validation (zero internal deps)
internal/room           per-room actor: seq, shapes, dedupe, resync, backpressure
internal/hub            room registry, connection budget, idle reaper
internal/ws             WebSocket upgrade, read/write pumps, origin policy
internal/api            routes, middleware, REST handlers, ops panel
internal/roomsvc        REST-facing room service (create/keys/snapshot)
internal/auth           room keys (sha256) and HMAC readonly tokens
internal/store/memory   zero-dependency store
internal/store/postgres durable store + embedded migrations + zstd snapshots
internal/broker/memory  single-instance broker
internal/broker/redis   Redis INCR seq + pub/sub fan-out
internal/webui          go:embed SPA with index.html fallback
internal/metrics        hand-written Prometheus exporter
internal/integration    end-to-end tests over real HTTP + WebSocket
```

## Design notes

**One goroutine owns a room.** `seq`, `shapes`, `members` and the dedupe ring are
touched only by the room's loop, so there is no room lock and broadcast order is a
structural property rather than a discipline. Everyone else talks to it through a
command channel.

**Nothing in a room ever blocks on a client.** Every outbound frame is a
non-blocking `Send`; a member that cannot accept a persistent frame is evicted with
close code 4001 and recovers through `resync`. A cursor or presence frame that does
not fit is simply dropped — a frozen cursor is invisible, a blocked room is not.

**All socket writes happen in the write pump.** `Kick` and close handshakes are
queued to the write pump rather than writing from the room goroutine: gorilla
allows one writer, and a second one both races and can block the event loop.

**Sequence numbers are the consistency backbone.** Single-instance mode increments
a local counter; multi-instance mode uses Redis `INCR` so seq is globally monotonic.
Out-of-order pub/sub frames are buffered and applied along a contiguous run, and a
room that is missing history replays the gap from the store (`room.CatchUp`) rather
than silently serving an incomplete canvas.

**Store before broadcast.** A client must never see a seq that cannot be replayed
after a restart. With `STORE=postgres` that costs one DB round trip per op (~1ms
locally); with `STORE=memory` it is free.

**Group commit.** Waiting for one DB round trip per op serialised ~1ms of store
latency onto the room goroutine for every op, which capped a Postgres-backed room at
~900 op/s and made a 500 op/s workload miss its latency budget. Ops are now
accumulated for at most 20ms (or 256 ops) and written in **one** statement per batch,
then broadcast. The invariants are unchanged: seq stays contiguous, and nothing is
broadcast before its batch is durable. Measured effect: 100 clients × 5 op/s,
30s — p95 40ms, p99 60ms, 100% delivery, zero errors (see `README` for how to
reproduce).

**Logs never contain personal data.** `key`, `rt`, `name` and `cid` are masked in the
access log. Nicknames exist on the wire (that is the feature) but never in a log
line.

**Metrics labels are a closed set.** Message types and error codes are enumerations
with `unknown`/`other` catch-alls, so a hostile client cannot grow the scrape
payload without bound.

## Modes

| `STORE` | `BROKER` | Behaviour |
|---|---|---|
| `memory` | `memory` | zero dependencies; rooms are lost on restart |
| `postgres` | `memory` | durable single instance (recommended for production) |
| `postgres` | `redis` | multi-instance: shared log, global seq, cross-instance fan-out |
| `memory` | `redis` | cross-instance but volatile — **load testing only**, see below |

## Deployment constraints (read before scaling)

**Running more than one instance requires `BROKER=redis` + `STORE=postgres`. Nothing
else is correct for more than one instance.**

Two separate reasons, both structural:

1. **Rooms live in the process.** With `STORE=memory`, instance B has no way to
   load a room created on instance A — the room only exists in A's heap. A client
   routed to B gets `404 room_not_found`. There is no fix at the broker layer;
   the room's very existence has to be durable. → `STORE=postgres`.
2. **Sequence gaps need a durable log to heal.** With `BROKER=redis`, seq comes
   from `INCR codraw:seq:{room}`. If an instance crashes *between* allocating a
   seq and publishing it, that seq is never used and every other instance sees a
   hole. The only repair is replaying the range from the store
   (`room.CatchUp`). With `STORE=memory` there is nothing to replay, so the hole
   is permanent: that instance's shape map stays incomplete until a client forces
   a full snapshot. → `STORE=postgres`.

`BROKER=redis` + `STORE=memory` therefore boots (the server logs a `WARN` naming
both settings and pointing at Postgres) but is **only** appropriate for a
throwaway load test with one writer. It is not a degraded production mode.

Single-instance deployments are unaffected: `STORE=memory BROKER=memory` is the
zero-dependency default, and `STORE=postgres BROKER=memory` is the recommended
durable single-node setup.

### Verifying multi-instance behaviour

`scripts/check-two-instances.sh` starts two real instances against Redis and
Postgres and asserts: both resolve the same room, seq is globally monotonic
(1,2,3) across alternating instances, the Redis counter matches, Postgres holds
one contiguous log, and both instances converge to the same shape count. It also
reads `joined.payload.instanceId` to prove the connections really landed on two
different processes — that field is emitted on every `joined` and repeated in
`/ops/data`.

Migrations are embedded and applied at startup under a Postgres advisory lock, with
a checksum per applied file so an edited migration fails loudly instead of drifting.

## 构建单文件二进制

前端产物经 `go:embed` 编译进二进制，产物是**一个可执行文件**，无需 Docker、Node 或静态目录。

```bash
# 仓库根目录
make binaries        # 前端构建 → 拷入 internal/webui/dist → 交叉编译
make verify-binaries

ls -lh dist/
# codraw-linux-amd64        23 MB  (ELF, statically linked)
# codraw-windows-amd64.exe  24 MB  (PE32+)
```

交叉编译已验证：`linux/amd64`、`linux/arm64`、`windows/amd64`、`darwin/amd64`、`darwin/arm64`。

健康检查用二进制自带子命令，不依赖 curl/wget：

```bash
./dist/codraw-linux-amd64 healthcheck
```

若 `internal/webui/dist` 为空（例如只 checkout 了后端），二进制仍能编译运行，`/` 返回一个说明页而不是崩溃 —— 这是为了并行开发时的可用性。
