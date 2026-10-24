## Summary

What does this pull request change, and why?

Closes #

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor / internal change
- [ ] Documentation
- [ ] Build, CI or tooling

## How it was tested

Describe the tests you ran (`go test ./...`, `make test-web`, manual steps).
For UI changes, include screenshots and say which devices you checked.

## Checklist

- [ ] `gofmt -l .` prints nothing, and `go vet ./...` and `go test ./...` pass
- [ ] Tests added or updated for the behavior change
- [ ] Database migrations are additive (no existing migration was edited)
- [ ] New dependencies use MIT-compatible licenses and are listed in `THIRD-PARTY-LICENSES.md`
- [ ] UI changes work with touch and mouse, and icon-only buttons have a `title`
- [ ] New user-facing mutations are classified in `api/automation/ui-action-manifest.v1.json`
- [ ] User-visible changes have an entry under `[Unreleased]` in `CHANGELOG.md`
- [ ] No secrets, private hosts/IPs or personal paths (the publish guard hooks passed)
