# Agent guide for scimux-team

`scimux-team` is the team server for scimux: a small store-and-forward relay
that single-seat scimux installations join. It is a separate binary and a
separate repository. Read `README.md` first. The wire contract is
`docs/protocol/team-v1.md`; a change to observable behavior changes that
document in the same commit.

## Commands

```sh
go build -o scimux-team ./cmd/scimux-team
go test -race ./...
gofmt -l . && go vet ./...
go run ./tools/covercheck
```

## Layout

- `cmd/scimux-team` is the thin executable: it calls `internal/team` and
  holds the linker-stamped version. Keep behavior out of it.
- `internal/team` owns the server and the operator commands. Other
  `internal/*` packages are leaf subsystems and never import `internal/team`.
- `internal/invite` generates and parses join codes. The domains are
  `scimux-team/join-verifier/v1` and `scimux-team/join-id/v1`.
- `internal/journal` is the shared append-only line file.
- `internal/registry` is the server identity, the join codes and the seats.
  They live in `registry.jsonl` inside the state directory.
- `tools/covercheck` is the coverage gate. It is not part of the product.
- `attic/` is untracked scratch. `.gitignore` is a deny-by-default allowlist
  for `*.md`; a new tracked document needs its own `!` line.

## This stage

- The state directory is `-state-dir`, default `$HOME/.scimux-team`. A
  missing directory is created mode `0700`, parents included. An existing
  directory is used only when its permission bits are already `0700`; its
  mode is not changed. `registry.jsonl` is mode `0600`. While a server is
  running it holds an exclusive non-blocking flock on `server.lock` in that
  directory. Operator
  commands do not take that lock.
- An active seat is one whose join code is redeemed. The limit is 200.
- `-url` is `http://host`, `https://host`, or the same with `:port` where
  the port is 1 to 65535. The scheme is lowercase. An IPv6 host is in
  brackets. User information, a path, a query and a fragment are rejected.
- `-name` follows the seat-name rule: 1 to 64 bytes of UTF-8, no control
  character, and no leading or trailing space. The default is `scimux team`.
  A name that breaks the rule is a usage error and the state directory is
  left untouched.
- A request body is limited to 16 MiB. The handler lock covers `Refresh`
  and the registry calls that follow it in that request. The lock is not
  held while a body is read or a response is written.
- The routes are `GET /v1/hello`, `POST /v1/register` and `GET /v1/seats`.
  Every other path, including every path under `/v1/channels`, answers `404`
  with the body `{"error":"not_found"}`.
- Fuzz targets are `FuzzParse` in `internal/invite` and `FuzzJournal` in
  `internal/journal`, one in each package.

## Invariants

- **Standard library only.** `go.mod` has no `require` block and there is no
  `go.sum`. A dependency is a design change that needs discussion.
- **Static binaries.** `CGO_ENABLED=0`. Linux, FreeBSD and macOS are the
  supported targets.
- **Store and forward, never interpret.** An entry body is an opaque JSON
  value. The server checks that it is valid JSON and within the size limit,
  and nothing else. It never parses, searches, summarizes or rewrites it.
- **The owner decides.** Only the owning seat writes a channel's mirror,
  title and member list. A seat that is neither the owner nor a listed member
  gets the same answer as for a channel that does not exist.
- **An installation ID is a public label, not a credential.** Only the token
  issued at registration authenticates a seat. A registered installation ID
  cannot be registered again until the operator revokes it.
- **Join codes are this server's own.** Only the `issue` command of
  `scimux-team` creates one. Each code admits one seat, once. Nothing issued
  by `scimux-rv` or any other service is ever valid here.
- **Operator commands match `scimux-rv`.** `issue`, `list`, `revoke` and
  `status`, the `-json` flag, the output shapes and the exit codes follow
  that tool. An operator who knows one knows the other.
- **The server keeps no secret it could reuse.** Join codes and tokens are
  stored as hashes only. A lost token is replaced by revoking the seat and
  registering again.
- **Logs are append-only and never rewritten in place.** A torn final line is
  ignored and removed before the next append. A damaged complete line is a
  hard error, never skipped. Removing a channel removes its files; that is
  the one deletion.
- **Nothing shared is logged.** No request log, no entry body, title, name,
  token or join code on stdout or stderr.
- **Plain HTTP by design.** TLS belongs to a proxy in front. Do not add
  certificate handling.
- **Fixtures are synthetic.** No real transcript content, host names, paths
  or identifiers in tests. Tests never run agent CLIs.
- **Coverage gate.** `go run ./tools/covercheck` must pass: every statement
  runs and every `if` is seen both ways in the package's own tests. A
  deviation needs a `// coverage-exempt: <reason>` comment at the place, and
  is acceptable only for a system call failure or a state that a test cannot
  provoke.
