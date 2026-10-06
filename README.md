# ecs-phoenix-ext

Custom Uptime Phoenix extension: Dell ECS storage usage dashboard /
wallboard. Phoenix core stays health-only for S3; ECS metering lives here,
in its own image.

- Go 1.25+, `CGO_ENABLED=0`, stdlib `net/http`, server-rendered templates.
- Polls `GET /object/billing/namespace/{ns}/info?include_bucket_detail=true&sizeunit=KB`
  (source of truth for usage) plus, best-effort per poll:
  `GET /object/bucket?namespace={ns}` (per-bucket block_size / notification_size)
  and `GET /object/namespaces/namespace/{ns}` (default_bucket_block_size).
  Inventory failures never flip `/health/ready` — usage stays live, the UI
  notes "Bucket info unavailable" instead.
- Size units are **binary** (API `GB` means GiB; Dell KB 000273649).
- Bucket quotas are ECS-native (Block Access thresholds from the bucket
  inventory poll) with 2-sample hysteresis before `/health/quota` goes 503.
  Namespace quota is also ECS-native now (the namespace-level `blockSize`
  threshold from `GET /object/namespaces/namespace/{ns}`). Quota-off buckets
  and namespaces show Unlimited; no quotas are operator-set in this tool.

## Dashboard columns (necessary only)

Bucket · Used (already includes incomplete multipart uploads) · Objects ·
Block · Notify are the ECS-native quota thresholds (UI: "Block Access at"
+ "Send Notification at", in GiB; API returns GiB and this tool normalizes
to bytes). Modes fall out of the two
numbers: Off (—/—) · Notification Only (—/X) · Block Only (X/—) ·
Block + Notify (X/Y); the dashboard labels the mode under the bucket name.
A 0/missing threshold means unset (quota off → Unlimited). The % bar and
/health/quota alert on the Block Access threshold; the Notify threshold
raises a yellow "notify threshold" warning.

## Local run (SQLite, no MariaDB needed)

```bash
export ECS_MGMT_URL=https://ecs.example.com:4443
export ECS_NAMESPACE=prod-ns
export ECS_USERNAME=ecs-usage
read -rs ECS_PASSWORD; export ECS_PASSWORD        # from your secret store
export DATABASE_DSN="file:./ecs-phoenix-ext.db"
export BASE_PATH=/                                # serve at / for go run

make run
# dashboard: http://localhost:8080/   (LISTEN_ADDR=:8080 for local)
# wallboard: http://localhost:8080/wallboard
```

Health probes (always open, no UI token):

```bash
curl -s localhost:8080/health/live
curl -s localhost:8080/health/ready    # 503 while ECS unreachable
curl -s localhost:8080/health/quota    # 503 when any bucket confirmed over
```

## Tests / lint

```bash
make test    # fixtures only; no live ECS needed
make vet
```

Live-ECS smoke tests are optional and guarded by `ECS_MGMT_URL` in the env;
default CI runs fixture tests only.

## Environment

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ECS_MGMT_URL` | yes | — | `https://ecs.example.com:4443`, no trailing slash |
| `ECS_NAMESPACE` | yes | — | Namespace to poll (one namespace in v1) |
| `ECS_USERNAME` | yes | — | Management user (`ecs-usage`, role NAMESPACE_ADMIN) |
| `ECS_PASSWORD` | yes | — | From Secret. Never logged |
| `ECS_SIZEUNIT` | no | `KB` | Query param `sizeunit` |
| `ECS_POLL_INTERVAL` | no | `15m` | Go duration, minimum `1m` |
| `ECS_TLS_CA_FILE` | no | — | Path to PEM |
| `ECS_TLS_CA` | no | — | Inline PEM |
| `ECS_TLS_INSECURE` | no | `false` | Lab only |
| `ECS_HTTP_TIMEOUT` | no | `60s` | Per management call |
| `LISTEN_ADDR` | no | `:80` | K8s container port |
| `DATABASE_DSN` | yes | — | MariaDB DSN **or** `file:/data/ecs-phoenix-ext.db` |
| `DATABASE_ENGINE` | no | inferred | `mariadb` or `sqlite` |
| `BASE_PATH` | no | `/storage` | Path prefix behind Ingress `/storage` |
| `PUBLIC_URL` | no | — | Optional absolute URL for logs |
| `UI_TOKEN` | no | empty | If set, dashboard/API/forms require `Authorization: Bearer …` or a `ui_token` query/form parameter. A valid credential is exchanged for an HttpOnly `SameSite=Lax` session cookie (`ecs_ui_session`, scoped to `BASE_PATH`) so the extension's own links and form posts keep working — this is what makes the Phoenix iframe flow work end-to-end. Empty means the UI is open to anyone who can reach it. Health stays open either way |
| `LOG_LEVEL` | no | `info` | slog level |

## MariaDB (dedicated schema and user, manual creation)

The plugin migrates its own `ext_ecs_usage_*` tables on start. MariaDB does
not treat a table-name fragment such as `ext_ecs_usage_%` as a prefix grant,
so the DBA provisions a **dedicated schema** instead of a grant on the
Phoenix application schema. Executable SQL is in `deploy/grants.sql`.
Substitute the password from your secret store before running it. Do not
reuse Phoenix's application credentials, and do not `GRANT` on `phoenix.*`.

`DATABASE_DSN` must select database `ecs_usage` (not the Phoenix application
schema). Schema-level rights cover tables added by later migrations; no
per-table grant update is required. If a dedicated schema is impossible,
`deploy/grants.sql` lists the current tables to grant explicitly — update
that list before deploying a migration that creates another table.

## Kubernetes shape

Separate Deployment (not a sidecar), Service, and an Ingress path
`/storage` **before** the catch-all `/`. See `deploy/manifests/`.
The standalone NetworkPolicy allows ECS `:4443`, DNS, and MariaDB
`:3306` to pods labeled `app.kubernetes.io/component: mariadb` in the same
namespace. For an external database, replace that pod selector with an
ipBlock of the database host only (see the comments in
`deploy/manifests/networkpolicy.yaml`) and set the port if it is not 3306.
Do not open `0.0.0.0/0`. Phoenix's own NetworkPolicy is not widened; the
chart-generated extension policy already permits database egress and is a
separate object.

Phoenix-side wiring (in the Phoenix chart overlay):

```yaml
extensions:
  - id: ecs-phoenix-ext
    title: Storage
    path: /storage
    image: ghcr.io/fiztoz/ecs-phoenix-ext:0.2.2
    port: 80
    readinessPath: /health/live
    database:
      secretName: ecs-phoenix-ext-db
```

`readinessPath` must be `/health/live`. That endpoint is serving readiness:
it stays 200 while the process can serve cached dashboard and wallboard
data. The chart default `/health/ready` is ECS dependency health and will
remove the only pod from Service endpoints during an ECS outage, which also
hides `/health/quota`. Keep pointing Phoenix HTTP monitors at the dependency
endpoint; do not use it as the Kubernetes probe.

Then add Phoenix HTTP monitors against:

- `{base}/health/ready` — ECS management plane unreachable
- `{base}/health/quota` — a bucket or namespace is confirmed over quota

### Phoenix iframe + UI_TOKEN

Phoenix embeds the dashboard in an iframe via its gated
`GET /api/extensions/{id}/frame` endpoint. When the Phoenix chart sets
`extensions[].uiToken` (rendered into Phoenix's `PHOENIX_EXTENSIONS`
Secret), the redirect arrives once as `{base}/?ui_token=…`; this server
swaps it for the `ecs_ui_session` cookie, and the wallboard link and quota
forms then work without the token reappearing in URLs. Only users holding
Phoenix's `can_view_extensions` capability ever receive the redirect. Note
that direct navigation to `{base}/` with `UI_TOKEN` set still requires the
Bearer header or token parameter — this extension authenticates itself;
Phoenix only gates discovery and launch.

For the sidebar tab icon, Phoenix can use `{base}/icon.svg` — a
stroke-drawn `currentColor` bucket with fill level, so it adapts to light
and dark sidebars.

Quota overage is not an outage: it must not flip the `s3` heartbeat DOWN.

## Docker

Local:

```bash
make docker            # ghcr.io/fiztoz/ecs-phoenix-ext:dev
```

CI (this repo) runs `go vet` + fixture tests, then builds and publishes to
GHCR on every push to `main` and on version tags:

| Ref | Tags |
|---|---|
| `main` | `:dev`, `:sha-<short>` |
| `v0.2.2` | `:0.2.2`, `:0.2`, `:latest` |

```bash
git tag v0.2.2
git push origin v0.2.2
```

After the first successful publish, make the package public once:
**https://github.com/users/fiztoz/packages/container/ecs-phoenix-ext/settings**
→ Change visibility → Public. New packages stay private until you do that.

Pull:

```bash
docker pull ghcr.io/fiztoz/ecs-phoenix-ext:dev
```

Final image is distroless static, non-root, read-only root fs. Build image
is `golang:1.25-alpine` (Go ≥ 1.25 is required by CGO-free
`modernc.org/sqlite`).

## Fixtures

`internal/ecs/testdata/namespace_info.{json,xml}` are synthetic, redacted
fixtures matching the documented field set. `bucket_list.json` and
`namespace_meta.json` are the same for the inventory endpoints (subset of
EMCECS/python-ecsclient `BUCKET` / `NAMESPACE` schemas: block_size,
notification_size in GiB, default_bucket_block_size in raw bytes, plus
namespace-level blockSize/notificationSize in GiB). Real ECS answers are XML
rooted at `<object_buckets>` / `<namespace>` with `-1` meaning unset, extra
elements (softquota, retention, search metadata, links) ignored. If you
capture real payloads from the lab, redact names if needed but keep the field
set, and replace these files.

## Nice to have (not planned yet)

Ideas deliberately parked; none are scheduled:

- **Usage history / trends** — a per-poll samples table with retention, so
  the wallboard could show sparklines instead of current state only.
- **Prometheus `/metrics`** — used-bytes gauge plus poll success/error
  counters, next to the existing health endpoints.
- **Multi-namespace support** — v1 polls exactly one namespace by design.
