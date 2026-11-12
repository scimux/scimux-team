# scimux-team

The team server for [scimux](https://github.com/scimux/scimux).

scimux runs on your own machine and stays a single-seat tool. `scimux-team`
is a small relay that a research group runs on its own infrastructure. A seat
that joins it can mirror a chat it chose to share. Other seats that the owner
invited can read the mirror and, if allowed, add comments. The server stores
and forwards. It never reads what it stores.

Status: repository skeleton. The wire contract is
[`docs/protocol/team-v1.md`](docs/protocol/team-v1.md). The server is not
implemented yet.

## Trust model

- The server speaks plain HTTP. Run it on a trusted network, or put a TLS
  proxy such as Caddy in front of it.
- Whoever operates the server can read everything that was shared with it.
- The owner of a chat decides who may read it and who may comment.

## Build and test

```sh
go build -o scimux-team ./cmd/scimux-team
go test -race ./...
go vet ./...
go run ./tools/covercheck      # statement and branch coverage gate
```

## Licence

GNU Affero General Public License, version 3. See `LICENSE`.
