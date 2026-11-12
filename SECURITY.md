# Security policy

## Reporting a vulnerability

Email **security@scimux.com**. Please do not open a public issue for a
security bug; the issue tracker is public, and so is everything in it.

Useful to include, if you have it: what an attacker gains, the steps to
reproduce, and which version you tested. A rough report you are unsure
about is still worth sending.

You will get a reply from a person within **5 working days**. This server
is maintained by one person, and that shapes what is worth promising: the
acknowledgement is a commitment, a fix date is not. If five working days
pass with no reply, assume the mail went astray. Resending is welcome, and
so is an issue that says only that you are waiting on a security mail.

## Supported versions

The latest GitHub release is supported. Until the first release is cut,
`main` is the release candidate. There are no backports.

## Before you start: what this server is

`scimux-team` is run by a research group on its own infrastructure. Three
facts are design, not findings:

- **It speaks plain HTTP.** It is meant for a trusted network or for a TLS
  proxy in front of it.
- **Its operator can read what was shared with it.** Entries are stored as
  they arrive.
- **It stores hashes of join codes and tokens, not the values.** A copy of
  the state directory still contains everything that was shared.

## What is in scope

- **Admission.** Registering without a valid join code, using a join code
  twice, registering an installation ID that is already registered, or
  acting as another seat.
- **Access to channels.** Reading or writing a channel without being its
  owner or a listed member, writing to a mirror without being its owner,
  commenting with read access only, or learning that a channel exists
  without access to it.
- **Revocation.** A revoked seat that keeps access, or a re-registered
  installation ID that inherits channels or memberships.
- **Storage.** Leaving the state directory through a crafted identifier,
  corrupting another channel's log, or passing a size or quota limit.
- **Leaks.** A join code, token, name, title or entry body in the server's
  output.

## What is not a vulnerability

- **Traffic that can be read on the network.** That is plain HTTP; use a
  trusted network or a TLS proxy.
- **The operator reading shared chats.**
- **Findings that assume access to the host or the state directory.**

## Disclosure

Coordinated: a fix should exist before the report is public, and the date
is agreed with you, not imposed.
