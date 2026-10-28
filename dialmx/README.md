# Standalone SMTP receiver

The optional standalone receiver is for operators who want SMTP receiving in a
separate container. For the self-operated choices and setup steps, including shared
Dial MX and private single-mode Remote MX; see the user guide for
[Remote SMTP receiving](../docs/REMOTE_MX.md).

From the repository root, the included Compose file builds and starts it:

```bash
docker compose -f dialmx/compose.yml up -d --build
```

Choose the mode and required settings in the guide before starting it. The
receiver does not store mailbox data; messages are retained by the Mammoth Mail
core.

Shared-mode authentication uses separate deadlines: up to 30 seconds for the
initial TXT lookup, 10 seconds for the core to answer an issued challenge, and
30 seconds total for fresh TXT/signature/MX verification. A timely response does
not expire while DNS verification is running. `MX_AUTH_TIMEOUT_SECONDS` (default
10) bounds each DNS lookup within those overall budgets. The core allows an
additional 25 seconds for frame writes and network scheduling before abandoning
either DNS phase. Timed-out or superseded attempts cannot install a late binding.
