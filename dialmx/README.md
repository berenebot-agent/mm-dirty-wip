# Standalone SMTP receiver

The optional standalone receiver is for operators who want SMTP receiving in a
separate container. For the supported choices and setup steps—including shared
Dial MX, Antler MX, and private single-mode Remote MX—see the user guide for
[Remote SMTP receiving](../docs/REMOTE_MX.md).

From the repository root, the included Compose file builds and starts it:

```bash
docker compose -f dialmx/compose.yml up -d --build
```

Choose the mode and required settings in the guide before starting it. The
receiver does not store mailbox data; messages are retained by the MailMoose
core.
