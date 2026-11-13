# scimux-team

The team server for [scimux](https://github.com/scimux/scimux).

scimux runs on your own machine and stays a single-seat tool. `scimux-team`
is a small relay that a research group runs on its own infrastructure. A seat
that joins it can mirror a chat it chose to share. Other seats that the owner
invited can read the mirror and, if allowed, add comments. The server stores
and forwards. It never interprets what it stores.

Status: a seat can join, rename itself and leave. Leaving has the same
effect as an operator revoke. An owner can share a chat. The operator
commands are `issue`, `list`, `revoke` and `status`. A seat registers
with a one-time join code and can list the registered seats. An owner
creates a channel, sets its title and member list, appends to its mirror
and removes it. A member reads the mirror and the comments. A member
with write access appends comments. A member can remove itself from one
channel. A channel can hold blobs. Revoking a seat, or the seat leaving,
removes the channels it owned and its memberships. The wire contract is
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
stderr. With `-url` it prints the join link `URL/join#CODE` instead. `-url`
is `http://host`, `https://host`, or the same with a port from 1 to 65535.
The scheme is lowercase. An IPv6 host is written in brackets. User
information, a path, a query and a fragment are rejected.

The server stores only a verifier. `-state-dir` defaults to `$HOME/.scimux-team`.
A missing state directory is created mode `0700`, including parents. An
existing directory is used only when its permission bits are already `0700`;
the tools do not change its mode.

Beside `registry.jsonl` the state directory holds one directory per
channel, `channels/<owner>/<chat>/`. That directory contains
`channel.json`, `mirror.jsonl`, `comments.jsonl` and `blobs/<id>`.
`channel.json` is replaced through a temporary file and a rename. The
two logs are append-only. A blob is one file of opaque bytes, named by
the SHA-256 of those bytes. It has no media type and no file name. It
is created once, through a temporary file and a rename, and is not
rewritten. Removing a channel removes its blobs. A blob stays when the
seat that uploaded it leaves the channel or the server, unless that
seat owned the channel. A damaged `channel.json` or log is left out of
the channel list and does not stop the server or any other channel;
requests for that channel itself fail. A damaged blob fails only that
blob. A seat's list loads only the logs of channels that seat owns or
belongs to. The index of a log keeps no entry body. A line that changes
after it was indexed fails a read of that channel. A log file is not
held open between calls, and neither is a blob file. The server that
holds `server.lock` is the only writer of a channel log. New directories
are mode `0700` and new files are mode `0600`.

Limits are constants. An entry body is at most 4 MiB, a JSON request is
at most 16 MiB and a log is at most 256 MiB. A blob is at most 4 MiB of
raw bytes. A longer upload is rejected after at most 4 MiB plus one
byte. A channel holds at most 4096 blobs and at most 256 MiB of blob
bytes. An owner has at most 1000 channels and a channel has at most 200
members. A read returns at most 256 entries and at most 16 MiB of
bodies, and at least one entry when one exists. There is no flag for
any of these.

`revoke` removes the seat. A running server drops that seat's channels
and memberships before it handles the next request. If that cleanup
fails, later requests try it again and answer internal until it
succeeds. Registration is refused, and spends no code, while the
cleanup is still due. A server that starts after a revocation does the
same cleanup before it listens. A later registration of the same
installation id starts with no channels and no memberships.

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
