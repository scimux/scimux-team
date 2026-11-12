# scimux-team

The team server for [scimux](https://github.com/scimux/scimux).

scimux runs on your own machine and stays a single-seat tool. `scimux-team`
is a small relay that a research group runs on its own infrastructure. A seat
that joins it can mirror a chat it chose to share. Other seats that the owner
invited can read the mirror and, if allowed, add comments. The server stores
and forwards. It never reads what it stores.

Status: a seat can join. The operator commands are `issue`, `list`, `revoke`
and `status`. A seat registers with a one-time join code and can list the
registered seats. Channels are not implemented yet. The wire contract is
[`docs/protocol/team-v1.md`](docs/protocol/team-v1.md).

## Trust model

- The server speaks plain HTTP. Run it on a trusted network, or put a TLS
  proxy such as Caddy in front of it.
- Whoever operates the server can read everything that was shared with it.
- The owner of a chat decides who may read it and who may comment.

## Run

```sh
scimux-team                                      # 127.0.0.1:8080
scimux-team -addr 127.0.0.1:8080 -state-dir ~/.scimux-team
scimux-team -version
scimux-team -state-dir ~/.scimux-team issue
scimux-team -state-dir ~/.scimux-team -url http://127.0.0.1:8080 issue
scimux-team -state-dir ~/.scimux-team list
scimux-team -state-dir ~/.scimux-team list --unused
scimux-team -state-dir ~/.scimux-team list --revoked
scimux-team -state-dir ~/.scimux-team revoke <id-or-installation-id>
scimux-team -state-dir ~/.scimux-team status
scimux-team -state-dir ~/.scimux-team -json list
```

`issue` prints the plaintext code once on stdout and the non-secret id on
stderr. With `-url` it prints the join link `URL/join#CODE` instead. The
server stores only a verifier. `-state-dir` defaults to `$HOME/.scimux-team`.
`-name` is the server name recorded on the first start. It follows the
seat-name rule: 1 to 64 bytes of UTF-8, no control character, and no
leading or trailing space. The default is `scimux team`. A name that
breaks the rule is a usage error and the state directory is left
untouched.

The listener binds loopback by default. Plain HTTP is for a trusted network
or a TLS proxy in front. The server does not terminate TLS. Only one server
may serve a state directory.

## Build and test

```sh
go build -o scimux-team ./cmd/scimux-team
go test -race ./...
go vet ./...
go run ./tools/covercheck      # statement and branch coverage gate
```

## Licence

GNU Affero General Public License, version 3. See `LICENSE`.
