# homelab DockerHub CI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** GitHub Actions workflow that builds multi-arch homelab Docker image on tag push and pushes to DockerHub.

**Architecture:** Single job using Docker Buildx with QEMU emulation to build linux/arm64 and linux/arm/v7 in one pass. Existing Dockerfile already supports TARGETARCH/TARGETVARIANT args.

**Tech Stack:** GitHub Actions, Docker Buildx, QEMU, DockerHub

## Global Constraints

- Trigger only on tags matching `v*`
- Platforms: `linux/arm64`, `linux/arm/v7`
- Image tags: `<username>/homelab:<git-tag>` and `<username>/homelab:latest`
- No changes to existing Dockerfile, Go code, or static files
- DockerHub credentials via GitHub Secrets (`DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`)
- Workflow file path: `.github/workflows/deploy.yml`

---

### Task 1: Create deploy.yml workflow

**Files:**
- Create: `.github/workflows/deploy.yml`

**Interfaces:**
- Consumes: GitHub Secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` (configured in repo settings post-deploy)
- Produces: DockerHub image tagged with git ref name + latest

- [ ] **Step 1: Create workflow file**

```yaml
name: Publish Docker image to DockerHub

on:
  push:
    tags:
      - 'v*'

jobs:
  build-and-push:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Set up QEMU
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to DockerHub
        uses: docker/login-action@v3
        with:
          username: ${{ secrets.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: .
          platforms: linux/arm64,linux/arm/v7
          tags: |
            ${{ secrets.DOCKERHUB_USERNAME }}/homelab:${{ github.ref_name }}
            ${{ secrets.DOCKERHUB_USERNAME }}/homelab:latest
          push: true
```

- [ ] **Step 2: Verify file structure**

Run: `ls -la .github/workflows/`
Expected: `deploy.yml` present

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/deploy.yml
git commit -m "ci: dockerhub publish workflow for homelab"
```
