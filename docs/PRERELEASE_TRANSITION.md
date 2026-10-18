# Pre-release database transition

**For pre-beta developer installations only. This is a one-time, offline
maintenance step, not a routine upgrade.**

Before the public beta, MailMoose's database grew from a chain of 80
pre-release migrations (`001`–`080`) into the single public baseline described
in this repository's `internal/dbschema/baseline.sql`. A database created by a
pre-release build **cannot** be upgraded by starting the server: the new
server refuses a pre-release lineage and points here. The conversion is done
**offline** by a separate maintenance utility while the application is stopped.

This guide covers, in order: preparation, backup, building the utility,
rehearsal, the actual conversion, starting the new server, and rollback.
Do one installation at a time, during a maintenance window. These are operator
instructions: this change does not deploy or execute them on either installation.
The operator selects the actual data path, backup location and maintenance window.

> **Commands below assume** the Compose service is named `mailmoose` and its
> data directory is `./data` (the repository's own `docker-compose.yml`). If
> your deployment uses another service name, container name, or a named volume
> instead of a bind mount, **adapt every command to your actual volume path** —
> do not blindly pass `./data`. The utility never assumes `/data` is a
> particular host path; you choose what to mount at `/data` inside it.

## 1. What the conversion changes

The utility takes an exact `080` database and rewrites the schema to the public
baseline, keeping all active application data. It exists because the pre-release lineage
carried tables, columns and indexes that were retired before the public schema
was frozen.

It removes retired objects:

- `api_keys`
- `api_key_mailbox_roles`
- `hermes_connections`
- the obsolete `idx_inboxes_id_account` index
- the private handoff columns `notification_status` and
  `notification_message_id` on `assistant_handling_requests`

It also:

- materialises a **non-null** `inboxes.storage_used_bytes` for any inbox whose
  counter is `NULL`, using the frozen `080` accounting (messages, drafts and
  draft attachments, remote metadata rows, remote folders and labels — exactly the sum
  the old build would have written), then rebuilds `inboxes` with the column
  `NOT NULL DEFAULT 0` and preserves every other column;
- replaces the retired per-migration `schema_migrations` chain with a single
  `001` marker and sets `PRAGMA user_version=1`.

It does **not** touch FTS (the full-text index and its shadow tables are left
as they are), message files under `/data/messages`, active API keys, stored
ciphertext, mailbox roles, events, the event sequence, or the mail MIME files.
The public client/credential model (`clients`, `client_api_keys`,
`client_inbox_bindings`, `client_push`) is preserved — those already replaced
the retired `api_keys`/`hermes_connections` tables at `080`.

The conversion outcomes are exercised by the automated tests under
`tests/unit/prerelease/` (they reconstruct the exact `080` schema and assert
what is retained and what is dropped), alongside the utility itself.

## 2. Before you stop the old server (preparation)

Do these on the **old build, while it is still running**, so there is something
to prepare:

1. **Sign in as the system administrator** in the web UI and confirm the login
   works. The conversion cannot recreate a lost login.
2. **Confirm your domains, providers and receiver settings are still saved**
   (Admin pages, each domain's sending/receiving configuration, the MX receiver
   settings). Persisted configuration is carried across; configuration that was
   only ever supplied through the environment is not. In particular, if the
   old deployment sets `MX_HOSTNAME`, save the intended hostname explicitly in
   Admin → MX receiver before cutover: a blank persisted hostname now uses
   `mailmoose-mx`, not an environment fallback.
3. **Reset any unsupported passwords.** The utility **rejects a
   database with an active password that is not Argon2id**; it will not convert
   one silently, and the conversion makes no crypto-format changes. A legacy
   PBKDF2 login cannot sign in on the final pre-release build either: reset an
   ordinary user's password with `mailmoose admin reset-password` and verify
   their login. Manage the system-administrator password through the existing
   `ADMIN_PASSWORD`/`ADMIN_PASSWORD_FILE` deployment setting, not that command.
   Do this before you stop; merely asking a PBKDF2 user to sign in cannot upgrade it.
4. **Finish the pre-release migrations.** Start the old build once more and let
   it settle so all `080` migrations are applied. The utility requires exactly
   the markers `001`–`080` and the captured `080` schema fingerprint; anything
   else is refused.
5. **Collect the encryption key.** If `APP_ENCRYPTION_KEY` is provided through
   Compose, a Docker secret, or an external secret store, have it to hand. If it
   is generated, it is the key file inside `/data` (see below). The conversion
   does **not** read or need the key, but the **new server does** — losing it
   makes every stored provider credential unreadable.

Record pending/uncertain outbound jobs and outstanding approvals. If you need a
rollback window with no new external activity, quiesce ingress, connectors and
outbound delivery using your deployment's existing controls. The new server
resumes configured workers immediately on startup; without external quiescence,
consider startup itself a resumption of traffic, not a read-only verification.

## 3. Back up everything (with the app stopped)

The DB will be rewritten in place, so the backup is not optional.
Run the following sections in the same Bash shell so the selected paths and
image pins stay available. Stop on any failed command; do not proceed without a
complete backup.

```bash
set -euo pipefail
umask 077
DATA_DIR="$(sudo realpath ./data)"  # replace with the actual bind-mounted directory
BACKUP_DIR="$HOME/mailmoose-backups/prebaseline-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p -m 700 "$BACKUP_DIR"   # outside the checkout: contains live secrets

# 1. Stop the application. The utility requires every writer to be stopped.
docker compose stop mailmoose

# 2. Copy the ENTIRE data directory, including SQLite WAL/SHM sidecars and the
#    generated key file. A clean stop may leave inbox.db-wal / inbox.db-shm.
#    Replace ./data with your actual data directory / volume.
sudo cp -a -- "$DATA_DIR" "$BACKUP_DIR/data"
cp -a -- .env docker-compose.yml "$BACKUP_DIR/"

# 3. Back up the encryption key and any external secrets SEPARATELY, so a later
#    restore never depends on the same copy as the data.
#    - If APP_ENCRYPTION_KEY is in Compose/.env or a Docker secret, save it, the
#      .env and compose file, and any secret files, outside the data backup.
#    - If MailMoose generated it, ./data/encryption_key is inside the copy above;
#      keep the separate copy of the value/file too.

# 4. Record and save the image the container is running, so you can restore it.
docker inspect --format '{{.Config.Image}} {{.Image}}' mailmoose > "$BACKUP_DIR/old-image.txt"
docker image save "$(docker inspect --format '{{.Image}}' mailmoose)" \
  | gzip > "$BACKUP_DIR/old-image.tgz"
```

Notes:

- **Back up with the container stopped.** WAL mode means a live copy of
  `inbox.db` alone can be inconsistent; the `-wal`/`-shm` files and a stopped
  writer are what make the copy clean.
- Host-side copies and `stat` below use `sudo` because `/data` normally belongs
  to the dropped runtime UID with mode `0700`. Omit it if already running as root.
- Keep the old image ID (or the saved tarball). If you later need the
  pre-release server and no longer have the image, rebuild it from the pinned
  pinned pre-release `080` commit — see §8.
- **Never point an old server binary at a converted database.** After
  conversion the database advertises `user_version=1`; an old build does not
  understand the public lineage. Roll back by restoring data, not by running the
  old image against converted data.

## 4. Build the offline utility image

The utility is a separate command (`cmd/prerelease-rebaseline`) linked into its
own image stage. It is **not** copied into the normal runtime image, so build it
explicitly from the revision you are upgrading to, at the repository root:

```bash
CUTOVER_REV="$(git rev-parse HEAD)"  # the reviewed post-baseline source revision
TOOL_IMAGE="mailmoose-rebaseline:public-$CUTOVER_REV"
SERVER_IMAGE="mailmoose:public-$CUTOVER_REV"
docker build --target prerelease-rebaseline -t "$TOOL_IMAGE" .
docker build -t "$SERVER_IMAGE" .
```

The resulting image's entrypoint is `/usr/local/bin/mailmoose-rebaseline`. It
links no application server, reads no key, performs no file cleanup, and makes
no network calls.

## 5. Rehearse on a copy first

Always rehearse the conversion on a copy of the backup, with the network
**off**, before touching the real data directory.

```bash
# A rehearsal directory is a full copy of the backup (including sidecars/key).
REHEARSAL_DIR="$BACKUP_DIR/rehearsal-data"
sudo cp -a -- "$BACKUP_DIR/data" "$REHEARSAL_DIR"

# Validate the copy offline. --check opens SQLite (it may create/remove WAL/SHM
# sidecars) but makes no logical changes.
docker run --rm --network none \
  --user "$(sudo stat -c '%u:%g' "$REHEARSAL_DIR/inbox.db")" \
  --mount type=bind,source="$REHEARSAL_DIR",target=/data \
  "$TOOL_IMAGE" --check
```

- Running the tool with `--network none` is deliberate: it can never reach a
  provider or send mail.
- **Never start the normal MailMoose app on the copy.** Even briefly, it would
  load live provider connections and its outbox and could send mail. Validate
  the copy only with the utility.
- A passing `--check` prints that the database is validated and makes no
  changes. Then rehearse the real conversion against the copy:

```bash
docker run --rm --network none \
  --user "$(sudo stat -c '%u:%g' "$REHEARSAL_DIR/inbox.db")" \
  --mount type=bind,source="$REHEARSAL_DIR",target=/data \
  "$TOOL_IMAGE" --apply
```

Delete the rehearsal directory once you are satisfied. Do not point the new
server at a rehearsal copy that has live credentials you are not ready to use.

## 6. Convert the real installation

Run the same two commands against the real, stopped data directory. `--check`
first, then `--apply`:

```bash
# Reuse the exact DATA_DIR backed up in §3; do not change it between steps.

# 1. Validate. No changes.
docker run --rm --network none \
  --user "$(sudo stat -c '%u:%g' "$DATA_DIR/inbox.db")" \
  --mount type=bind,source="$DATA_DIR",target=/data \
  "$TOOL_IMAGE" --check

# 2. Convert. Fully transactional: either the whole rebaseline commits or
#    nothing does.
docker run --rm --network none \
  --user "$(sudo stat -c '%u:%g' "$DATA_DIR/inbox.db")" \
  --mount type=bind,source="$DATA_DIR",target=/data \
  "$TOOL_IMAGE" --apply
```

You must choose **exactly one** of `--check` or `--apply`; the tool exits with
a usage error otherwise. `--data-dir` defaults to `/data`, which is why the
commands above only mount the directory.

### What `--apply` does, and its guarantees

- It runs **fully transactionally**. It disables foreign-key enforcement
  (`PRAGMA foreign_keys=OFF`) **outside** the `BEGIN IMMEDIATE` transaction,
  restores it afterwards, and validates the final schema, `PRAGMA
  integrity_check` and `PRAGMA foreign_key_check` **before committing**. If
  anything fails, the transaction rolls back and the database is left as it was.
- It converts only a database whose migration markers are exactly `001`–`080`
  and whose schema matches the captured `080` fingerprint. There is **no
  support for arbitrary older chains** — go through one supported pre-release
  build first if you are further back.
- It refuses (rather than repairs) these before converting:
  - a missing or non-regular `inbox.db`;
  - an incomplete or unexpected migration sequence;
  - a schema that does not match the captured `080` fingerprint;
  - an integrity or foreign-key problem;
  - inconsistent client/credential/connector ownership;
  - an active password that is not Argon2id;
  - live ciphertext that is not the supported `v2.` format (retired copied
    rows, such as the ones on the dropped `hermes_connections` table, are not
    checked).
- An **already-converted** database (public baseline, `user_version=1`) is
  validated and left alone: both `--check` and `--apply` are no-ops that simply
  verify it. Running the utility twice is safe.

## 7. Start the new server

Start exactly the post-conversion image built in §4, not an unverified `latest`
registry tag. For the example Compose deployment, pin it with an override:

```bash
cat > "$BACKUP_DIR/public-image.yml" <<EOF
services:
  mailmoose:
    image: $SERVER_IMAGE
    pull_policy: never
EOF
docker compose -f docker-compose.yml -f "$BACKUP_DIR/public-image.yml" up -d --no-build --pull never
docker compose logs --tail 100 mailmoose
```

The new image creates the public baseline on an empty database and accepts an
already-converted one; it **refuses** a pre-release lineage and tells you to run
this guide. Confirm the administrator can sign in and that a domain's stored
providers and receiver settings are intact. Verify an existing scoped API key,
message search, attachments, labels and event replay; inspect pending and
uncertain sends without blindly retrying them. Keep using this override, or persist the same reviewed image pin in
your deployment configuration, before any later restart. Complete and verify
the first installation before repeating the procedure for the second.

## 8. Rollback

Roll back **before** real traffic resumes on the new server, if at all possible.
Use the immediate restart sequence below only if no external processing has
resumed. Otherwise preserve the converted state and reconcile first: an old
snapshot may contain pending jobs that the new server has already sent.

```bash
docker compose down

# Restore the ENTIRE data backup from §3, including -wal/-shm and the key file.
# Do NOT merge: replace the data directory with the backup copy.
sudo mv -- "$DATA_DIR" "$BACKUP_DIR/converted-data-$(date -u +%Y%m%dT%H%M%SZ)"
sudo cp -a -- "$BACKUP_DIR/data" "$DATA_DIR"

# Point the Compose service back at the pre-conversion image and start it
# without building from the current (post-conversion) checkout. Tag the image
# ID recorded in §3 (or the one you rebuilt from the pinned commit) under the
# name your Compose file resolves.
OLD_IMAGE="$(awk '{print $2}' "$BACKUP_DIR/old-image.txt")"
docker image tag "$OLD_IMAGE" mailmoose:prebaseline-080-rollback
cat > "$BACKUP_DIR/rollback-image.yml" <<EOF
services:
  mailmoose:
    image: mailmoose:prebaseline-080-rollback
    pull_policy: never
EOF
docker compose -f docker-compose.yml -f "$BACKUP_DIR/rollback-image.yml" up -d --no-build --pull never
```

If you kept only the tarball, `docker load < "$BACKUP_DIR/old-image.tgz"` first,
then tag the restored image ID. If your Compose file resolves the service from a
registry tag rather than a local image, pin that tag to the pre-conversion image
for the rollback instead.

- Roll back with the **whole data directory plus the same encryption key plus
  the old image**. Restoring only `inbox.db` risks a mismatched key or a stale
  WAL.
- Rebuild the old image from the pinned commit if you did not keep it, without
  checking out a branch:

  ```bash
  git archive 9b6ae04 | docker build -t mailmoose:prebaseline-080 -
  ```

  `9b6ae04` is the pre-release `080`/pre-rebaseline revision used to capture the
  supported schema.
- **After traffic has resumed on the new server, any rollback needs a manual
  reconcile of mail received since cutover.** That mail exists only in the
  converted database; restoring the old backup discards it, so anything
  received after cutover must be re-imported or manually accounted for.
- **Reconcile outbound and connector state before restarting restored workers.**
  An old pending send or approval notification may already have been accepted
  after cutover; restoring its old queue state can send it twice. Compare the
  retained converted database and provider evidence, preserve uncertain-delivery
  quarantine, and reconcile relay/webhook cursors and refreshed provider grants.
  Do not restart an unpaused old server or automatically retry uncertain sends
  until that review is complete.

## 9. What is not involved

- **No new infrastructure or dependencies.** The utility runs from the same
  image build, offline, as a local process.
- **No encryption key is read.** The utility does not accept, look up, or write
  `APP_ENCRYPTION_KEY` and never decrypts anything.
- **No mail-content/key file changes, network, sweep or config-import activity.**
  Only SQLite and its journal/sidecars are written. It does not touch
  `/data/messages`, draft/workflow files, run delivery cleanup, contact providers, or import
  environment configuration. Existing MX receiver settings remain authoritative
  — the separate standalone receiver's own environment (see
  [Remote SMTP receiving](REMOTE_MX.md)) is untouched.
