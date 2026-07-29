# homelab DockerHub CI Workflow

Deploy the homelab (armbian-stats-web) Docker image to DockerHub via GitHub Actions when version tags are pushed.

## Trigger

- Git tag matching `v*` (e.g., `v1.0.0`, `v2.3.1`)
- Push to `main` does NOT trigger deployment (only tags)

## Workflow: `deploy.yml`

Single job `build-and-push` runs on `ubuntu-latest`.

### Steps

| # | Action | Purpose |
|---|--------|---------|
| 1 | `actions/checkout@v4` | Check out repo at the tagged commit |
| 2 | `docker/setup-qemu-action@v3` | Register QEMU static binaries for cross-platform emulation (enables building arm on x86) |
| 3 | `docker/setup-buildx-action@v3` | Create Buildx builder with multi-platform support |
| 4 | `docker/login-action@v3` | Authenticate to DockerHub using GitHub Actions secrets |
| 5 | `docker/build-push-action@v6` | Build multi-arch image, push to DockerHub |

### Platforms

- `linux/arm64`
- `linux/arm/v7`

### Image tags on DockerHub

```
<username>/homelab:<git-tag>   # e.g., v1.0.0
<username>/homelab:latest      # always points to latest tagged release
```

### Dockerfile compatibility

Existing Dockerfile already defines `TARGETOS`, `TARGETARCH`, `TARGETVARIANT` ARGs and uses them in the build stage. Buildx sets these automatically per-platform — no changes needed.

## Secrets (configured in GitHub repo)

| Secret | Value |
|--------|-------|
| `DOCKERHUB_USERNAME` | DockerHub username |
| `DOCKERHUB_TOKEN` | DockerHub access token (not password) |

## Files

New file only: `.github/workflows/deploy.yml`

## Security

- Use DockerHub access tokens (scoped, revocable) not passwords
- Secrets stored in GitHub, never in code
- No privileged permissions required in workflow

## Verification

After first tagged push, verify:
1. DockerHub repo contains `homelab` image with both arm64 and arm/v7 manifests
2. `docker manifest inspect <username>/homelab:latest` shows both platforms
3. Pull and run on target Armbian device
