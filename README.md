# ecs-phoenix-ext

Custom Uptime Phoenix extension: Dell ECS storage usage dashboard /
wallboard. Phoenix core stays health-only for S3; ECS metering lives here,
in its own image.

- Go 1.23+, `CGO_ENABLED=0`, stdlib `net/http`, server-rendered templates.
- Polls `GET /object/billing/namespace/{ns}/info?include_bucket_detail=true&sizeunit=KB`.
- Size units are **binary** (API `GB` means GiB; Dell KB 000273649).
- Quotas are operator-set in this UI (ECS has no bucket quota field) with
  2-sample hysteresis before `/health/quota` goes 503.

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
| `UI_TOKEN` | no | empty | If set, dashboard/API/forms require `Authorization: Bearer …`. Empty means the UI is open to anyone who can reach it. Health stays open either way |
| `LOG_LEVEL` | no | `info` | slog level |

## MariaDB (dedicated user, manual creation)

The plugin migrates its own `ext_ecs_usage_*` tables on start. The DBA
creates the user once — Phoenix never does, and the plugin user only ever
gets grants on its own prefix:

```sql
CREATE USER 'ecs_usage'@'%' IDENTIFIED BY '<choose-a-strong-value>';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
  ON phoenix.ext_ecs_usage\_% TO 'ecs_usage'@'%';
FLUSH PRIVILEGES;
```

`DATABASE_DSN` then is the DSN for that user only. Never mount the Phoenix
application DSN into this pod.

## Kubernetes shape

Separate Deployment (not a sidecar), Service, and an Ingress path
`/storage` **before** the catch-all `/`. See `deploy/manifests/`.
Extension NetworkPolicy allows egress to ECS `:4443` only; Phoenix's own
NetworkPolicy is not widened.

Phoenix-side wiring (in the Phoenix chart overlay):

```yaml
extensions:
  - id: ecs-phoenix-ext
    title: Storage
    path: /storage
    image: ghcr.io/fiztoz/ecs-phoenix-ext:0.1.0
    port: 80
    database:
      secretName: ecs-phoenix-ext-db
```

Then add Phoenix HTTP monitors against:

- `{base}/health/ready` — ECS management plane unreachable
- `{base}/health/quota` — a bucket is confirmed over quota

Quota overage is not an outage: it must not flip the `s3` heartbeat DOWN.

## Docker

Local:

```bash
make docker            # ghcr.io/fiztoz/ecs-phoenix-ext:dev
```

CI (this repo) builds and publishes to GHCR on every push to `main` and on
version tags:

| Ref | Tags |
|---|---|
| `main` | `:dev`, `:sha-<short>` |
| `v0.1.0` | `:0.1.0`, `:0.1`, `:latest` |

```bash
git tag v0.1.0
git push origin v0.1.0
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
fixtures matching the documented field set. If you capture real payloads
from the lab, redact names if needed but keep the field set, and replace
these files.
