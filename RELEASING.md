# Releasing the Agent

GitHub releases are automated by workflow:

- `.github/workflows/agent-release.yml`

## Release Targets

- `linux/amd64`
- `linux/arm64`
- `windows/amd64`
- `windows/arm64`
- `darwin/arm64`

## Create a Release

1. Create and push a tag:

```bash
git tag agent-v1.0.0
git push origin agent-v1.0.0
```

2. GitHub Actions will:
- run tests
- build cross-platform binaries
- package archives
- generate `SHA256SUMS.txt`
- publish a GitHub Release with all assets

## Manual Trigger

You can also run the workflow manually with `workflow_dispatch` and provide `version` (for example `1.0.1`).

## Verify Binary Version

Release binaries embed version via `-ldflags`, and expose:

```bash
./mon-agent -version
```
