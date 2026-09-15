# Contributing to Relay

Thanks for helping improve Relay! Bug reports, documentation fixes and code
changes are all welcome.

## Before you start

- **Security issues:** don't open a public issue. Follow [SECURITY.md](SECURITY.md).
- **Bugs:** open an issue with the Relay version (Settings → About), the proxy
  engine (nginx or Relay Edge), what you did, what you expected and what
  happened. Relevant logs help a lot; remove secrets, tokens and passwords first.
- **New features or big changes:** open an issue to discuss the idea first,
  so no work is wasted on something that doesn't fit Relay's scope.

## Development setup

You need Go (see `go.mod` for the version), Node.js with npm, and Docker with
Compose to run the full stack.

```bash
git clone https://github.com/ollieInstantOffr/app-relay.git
cd app-relay

make dev                 # API on :8181 with ./data (engines unavailable without compose)
cd web && npm install && npm run dev    # UI on :5173, proxies /api and /mcp to :8181
```

To run the whole stack (Relay, nginx, Relay Edge and HAProxy):

```bash
make up
```

Use a separate clone for development if you also run Relay for real: **Settings
→ Updates → Upgrade** builds from the checkout it runs from, including
uncommitted changes.

### Where things live

- `cmd/relay`: the `relay` binary (server, agents, Relay Edge, CLI)
- `internal/model`: configuration entities; `internal/store`: SQLite
- `internal/render/{nginx,edge,haproxy}`: config renderers; `internal/agent`: engine control protocol
- `internal/edge`: Relay Edge, the built-in reverse proxy (`relay edge run`)
- `internal/apply`: pending changes, versions, validation, reload, auto-rollback
- `internal/publicdns`, `internal/mcp`, `internal/auth` …: one package per feature area
- `web/`: React + TypeScript UI (built into `internal/webui/dist`, embedded in the binary)
- `docs/SLICES.md`: architecture contracts between feature areas

Run the tests with `make test`.

## Making a change

1. Fork the repository and create a branch from `main`.
2. Keep the change focused: one fix or feature per pull request.
3. Match the style of the surrounding code (naming, comments, structure).
   Run `gofmt` on Go code.
4. Add or update tests for what you changed.
5. Update the in-app documentation (`web/src/features/docs/content/`) when
   behaviour or the UI changes.
6. Check that everything passes:

   ```bash
   go build ./... && go vet ./... && go test ./...
   cd web && npx tsc -b
   ```

7. Open a pull request that explains **what** changed and **why**, and how you
   tested it. Screenshots help for UI changes.

### UI guidelines

- Use the app's own components in `web/src/components/ui`. Don't use
  OS-native form controls (native `<select>`, checkboxes, radios, colour
  pickers).
- Keep copy short, plain and friendly.
- Make sure pages work for read-only roles (viewer) as well as admins.

### Configuration and security

- Changes to configuration rendering (nginx, Relay Edge, HAProxy) must keep
  working with both proxy engines and pass their validation (`nginx -t` /
  `relay edge check`).
- Never log or return secrets. Credentials must stay masked in API responses.
- Never commit secrets, tokens, private keys or personal data. This repository
  is public.

## Commit messages and versions

Relay's version number is calculated from commit messages
(`scripts/version.sh`), so please use these prefixes:

| Commit | Version bump | Example |
|---|---|---|
| `fix: …` (or any other prefix) | patch: 0.5.3 → 0.5.4 | `fix: keep maintenance page on static assets` |
| `feat: …`, or `[minor]` in the subject | minor: 0.5.3 → 0.6.0 | `feat: add Cloudflare to Public DNS` |
| `feat!: …` / `fix!: …`, or `[major]` in the subject | major: 0.5.3 → 1.0.0 | `feat!: remove the legacy API` |

Keep the subject line short and in the imperative ("add", "fix", not "added").

## License

Relay is licensed under the [GNU Affero General Public License v3.0](LICENSE).
By contributing, you agree that your contribution is licensed under the same
license.
