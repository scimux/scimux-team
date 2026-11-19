# scimux team protocol, version 1

This is the wire contract between a scimux seat and a `scimux-team` server.
It is plain HTTP with JSON bodies. Seats poll; there is no push channel.

## 1. Terms

- **Seat.** One scimux installation. It is named by its installation ID: 32
  lowercase hexadecimal characters. The ID is a public label, not a
  credential.
- **Token.** The secret a seat receives at registration: 64 lowercase
  hexadecimal characters. It authenticates every later request.
- **Channel.** One shared chat. It is named by the owning seat's
  installation ID and a chat ID that matches `[A-Za-z0-9_-]{1,64}`.
- **Mirror.** The channel's append-only log of the owner's chat records.
- **Comments.** The channel's append-only log of what members wrote.
- **Entry.** One record in a log. Its body is an opaque JSON value.
- **Blob.** An unchangeable byte string of 1 byte to 4 MiB that belongs
  to a channel. It is named by the SHA-256 of its bytes: 64 lowercase
  hexadecimal characters.

## 2. Conventions

- Every path below is under the server's base URL.
- A request with a body sends one JSON object. Unknown fields, a second JSON
  value after the object and a body over 16 MiB are rejected. The one
  exception is a blob upload, which sends raw bytes.
- A response with a body is `application/json` and carries
  `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. The one
  exception is a blob download, which is `application/octet-stream` with
  the same two headers.
- Times are UTC in the form `2026-11-12T09:30:00.000Z`.
- Authentication is `Authorization: Bearer <token>`.
- A path or method that this document does not list answers `404 not_found`.

An error response is `{"error":"<code>"}` with one of these codes:

| Status | Code | Meaning |
| --- | --- | --- |
| 400 | `bad_request` | Malformed body, field, path part or query value |
| 401 | `unauthorized` | Missing, unknown or revoked token |
| 403 | `join_refused` | Unknown, used, expired or revoked join code |
| 403 | `forbidden` | The caller can see the channel but may not do this |
| 404 | `not_found` | No such route, or no channel the caller can see |
| 409 | `already_registered` | The installation ID is registered |
| 409 | `stale_generation` | An append or deletion names another generation than the channel has |
| 413 | `too_large` | Body over 16 MiB, an entry body over 4 MiB, or a blob over 4 MiB |
| 422 | `unknown_member` | A member list names a seat that is not registered |
| 500 | `internal` | The server could not read or write its state |
| 507 | `quota` | A seat, channel, log or blob limit is reached |

Checks run in this order: authentication, then whether the caller can see
the channel, then whether the caller may do this, then the request content.
A channel that exists but that the caller neither owns nor is a member of
answers exactly like a channel that does not exist.

## 3. Join link

The operator issues one join code per seat with the server's own `issue`
command. No other tool or service can issue a code for this server; a code
from `scimux-rv` is never valid here. With the server's address it forms a
join link:

```
http://relay.example:8080/join#0123-4567-89AB-CDEF-GHJK-MNPQ
```

The part before `/join` is the base URL. The part after `#` is the join
code: 24 characters of Crockford Base32, shown in groups of four. A reader
ignores `-` and spaces, accepts lowercase, and maps `O` to `0` and `I` or
`L` to `1`.

A join code admits exactly one seat, once. After a successful registration
it is used up. An unused code expires 30 days after it was issued, and the
operator can revoke it earlier.

The server has no `/join` route. The link is a carrier for the address and
the code; the seat takes it apart and never requests it. The part after `#`
is not sent to a server even when a person opens the link in a browser.

## 4. Routes without authentication

### `GET /v1/hello`

```json
{"protocol":1,"server":{"id":"<32 hex>","name":"Lab relay"}}
```

### `POST /v1/register`

```json
{"join_code":"0123-4567-89AB-CDEF-GHJK-MNPQ","installation_id":"<32 hex>","name":"Berger"}
```

`name` is 1 to 64 bytes of UTF-8 without control characters and without
leading or trailing space. Names need not be unique.

Answers `201`:

```json
{"token":"<64 hex>","server":{"id":"<32 hex>","name":"Lab relay"}}
```

The token is shown once and cannot be retrieved again. The join code is
checked first, so `already_registered` is only told to a caller that holds a
valid join code. A refused registration does not use up the code.

## 5. Seats

### `GET /v1/seats`

All registered seats, ordered by ID. Revoked seats are absent.

```json
{"seats":[{"id":"<32 hex>","name":"Berger"}]}
```

### `PUT /v1/seat`

Changes the caller's own name.

```json
{"name":"Berger at the lab"}
```

`name` follows the rule of registration. Answers `200`:

```json
{"id":"<32 hex>","name":"Berger at the lab"}
```

`GET /v1/seats` shows the new name at once. Entries name their author by
ID, so nothing else changes.

### `DELETE /v1/seat`

The caller leaves the server. The request body is not read. Answers `204`.

Leaving has the effect of a revocation, section 8: the token stops working,
the seat disappears from `GET /v1/seats`, the server removes the channels
the seat owned and removes the seat from every member list. The operator
sees the seat's join code as revoked. The same installation ID can register
again with a new join code and then starts with nothing.

If the server cannot finish removing the channels, it answers `500`. The
seat is gone all the same; a repeated request answers `401`, and the server
finishes the removal before it serves another request.

## 6. Channels

A channel object:

```json
{
  "owner": "<32 hex>",
  "chat": "a1b2c3d4e5f60718",
  "title": "feat-multiplayer-3",
  "generation": "<16 hex>",
  "created_at": "2026-11-12T09:30:00.000Z",
  "access": "owner",
  "mirror_last": 42,
  "comments_last": 3,
  "members": [{"seat":"<32 hex>","access":"read"}]
}
```

`access` is the caller's access: `owner`, `write` or `read`. `mirror_last`
and `comments_last` are the highest sequence numbers, or `0` for an empty
log. `generation` is chosen at random when the channel is created. A client
that sees a different generation for a known channel discards its read
positions for it. `members` is present only when the caller is the owner;
it is ordered by seat.

### `GET /v1/channels`

Every channel the caller owns or is a member of, ordered by owner, then
chat. This is the polling call.

```json
{"channels":[ ... ]}
```

### `PUT /v1/channels/{owner}/{chat}`

Owner only. Creates the channel, or changes its title.

```json
{"title":"feat-multiplayer-3"}
```

`title` is 0 to 200 bytes of UTF-8 without control characters. Answers `201`
with the channel object when created and `200` when it already existed.

### `DELETE /v1/channels/{owner}/{chat}`

Owner only. Requires `generation` as a query value of 16 lowercase hexadecimal
digits. A missing, malformed or repeated value is `bad_request`. A generation
that differs from the current channel generation answers `stale_generation`
and deletes nothing. Authentication, visibility and permission checks precede
the generation check. The comparison and deletion run under the same lock
as channel creation and appends.

A matching generation removes the channel, both logs and its blobs and
answers `204`. Creating the same channel again gives it a new generation and
empty logs.

### `PUT /v1/channels/{owner}/{chat}/members`

Owner only. Replaces the whole member list.

```json
{"members":[{"seat":"<32 hex>","access":"read"},{"seat":"<32 hex>","access":"write"}]}
```

At most 200 members. `access` is `read` or `write`. A seat listed twice, or
the owner listed, is `bad_request`. A seat that is not registered is
`unknown_member`. Answers `200` with the channel object.

### `DELETE /v1/channels/{owner}/{chat}/membership`

Members only. The caller removes itself from the member list. Answers
`204`. The owner gets `forbidden`. Entries the member wrote stay.

## 7. Logs

The two logs of a channel have the same shape:

- `/v1/channels/{owner}/{chat}/mirror`
- `/v1/channels/{owner}/{chat}/comments`

| | Read | Append |
| --- | --- | --- |
| Mirror | owner, `read`, `write` | owner |
| Comments | owner, `read`, `write` | owner, `write` |

### Append: `POST` on a log

```json
{"generation":"<16 hex>","entries":[{"key":"0/17","body":{"any":"json"}}]}
```

- `generation` is required on both logs. It is 16 lower-case hexadecimal
  digits. A missing or malformed value is `bad_request`.
- When it is not the generation of the channel, the server answers
  `stale_generation` and stores nothing.
- This check is part of the request content, so it runs after
  authentication, visibility and permission, as section 2 orders the
  checks. A caller that cannot see the channel still gets `not_found`,
  and a caller without the right still gets `forbidden`.
- The comparison and the append run under the same lock as deleting and
  creating the channel. An append cannot pass the check and then land in
  a channel created after the check.
- 1 to 256 entries.
- `key` matches `[A-Za-z0-9._:/-]{1,200}`. It makes a retry safe. In the
  mirror a key is unique per channel. In the comments a key is unique per
  author.
- `body` is any JSON value. The server stores it in compact form and never
  interprets it. The compact form is at most 4 MiB.

The server gives each new entry the next sequence number, starting at 1, and
the current time. An entry whose key is already stored is not stored again;
the first body wins. The request is all or nothing: if one entry is invalid,
or the log limit would be passed, nothing is stored.

Answers `200`:

```json
{"generation":"<16 hex>","results":[{"key":"0/17","seq":43,"duplicate":false}],"last":43}
```

A client that receives `stale_generation` treats the channel as replaced.
It discards its read positions for that channel and does not send the
batch again.

### Read: `GET` on a log

The query value `after` is a sequence number and defaults to `0`.

Answers `200` with the entries whose sequence number is greater than
`after`, in order, at most 256 of them:

```json
{
  "generation": "<16 hex>",
  "entries": [
    {"seq":43,"key":"0/17","author":"<32 hex>","at":"2026-11-12T09:30:00.000Z","body":{"any":"json"}}
  ],
  "last": 43,
  "more": false
}
```

`author` is the seat that appended the entry. `last` is the highest
sequence number in the log. `more` is true when entries after the returned
ones exist. A response carries at most 16 MiB of bodies, and always at
least one entry when one exists.

## 8. Revocation

The operator revokes a seat by revoking the join code it registered with.
Its token stops working at once, it
disappears from `GET /v1/seats`, the server removes the channels it owned,
and the server removes it from every member list. The same installation ID
can then register again with a new join code. It starts with no channels
and no memberships, so a new holder of the ID inherits nothing.

## 9. Blobs

A blob carries an image or an attachment that an entry refers to. The
server never interprets it and stores no media type and no file name; the
entry that refers to the blob carries those.

| | Read | Write |
| --- | --- | --- |
| Blob | owner, `read`, `write` | owner, `write` |

The successful hello response advertises `Scimux-Blob-Generation: 1` as an
HTTP header. The hello JSON shape is unchanged; clients that require fenced
blob operations must verify the server identity and this capability before
issuing a blob request. The header is not an authentication credential.

Blob PUT and GET accept an optional `generation` query parameter. When
present it must occur once and be 16 lowercase hexadecimal characters. Access
is checked first; the generation must match the current channel. PUT checks
again after reading the body under the same serialization as channel changes.
A mismatch returns `stale_generation`. Calls without the parameter retain
legacy behavior and do not provide this fence.

### `PUT /v1/channels/{owner}/{chat}/blobs/{id}`

The body is the raw bytes, 1 byte to 4 MiB. `id` is the SHA-256 of the
body. Answers `201` when the blob is new and `200` when it was already
stored, both with:

```json
{"id":"<64 hex>","size":20481}
```

An `id` that is not 64 lowercase hexadecimal characters, an empty body and
a body whose SHA-256 is not `id` are `bad_request`. A body over 4 MiB is
`too_large`. A channel holds at most 4096 blobs and at most 256 MiB of
blob bytes; an upload that would pass either limit is `quota`.

### `GET /v1/channels/{owner}/{chat}/blobs/{id}`

Answers `200` with the bytes. A blob that is not stored is `not_found`.

A blob cannot be changed or removed on its own, and blobs cannot be
listed. Removing the channel removes its blobs. A blob stays when the seat
that uploaded it leaves the channel, leaves the server or is revoked,
unless that seat owned the channel. An unreferenced blob, including one left
after an interrupted publication, remains within the channel limits until
channel removal; the relay does not interpret entries as file references.
