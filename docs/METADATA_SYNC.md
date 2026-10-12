# Live metadata sync status

The existing header Queue includes one status row per visible remote inbox while
metadata work is queued, running, unfinished, paused or needs attention. Every
folder view of that inbox shows the same inbox-wide progress at the top. Completed
work disappears on the next queue poll. This covers Gmail, One, Outlook and IMAP;
hosted/local inboxes do not require a remote metadata download.

Progress counts durable cached metadata rows, not attempted requests. Message
bodies and attachment bytes are not downloaded by this work. Gmail's multiple
label memberships do not multiply its indexed message count. IMAP counts folder
message records, matching the protocol's per-folder UID identity.

When a total is available from existing provider enumeration, the UI displays
indexed/total and a percentage. Outlook totals come from the enumerated folders'
item counts; IMAP totals come from complete UID snapshots (or a verified unchanged
folder's cache count). Gmail/One totals come from the provider profile's
`messagesTotal` and are marked as an **estimate** (`N / ~M`): the profile counts
every label including Spam/Trash while the backfill indexes a folder subset, so
the denominator is approximate. Normalisation guards the fraction: the denominator
is raised to meet the indexed count (so a pass can never render 110/100), the
percentage is clamped to 0-100, and a completed pass snaps the denominator to the
indexed count so the final paint reads M/M rather than a sub-100 plateau. A total
of 0 means "unknown" and the row shows a bare count.

Queued/running status reflects this process's actual workers and short metadata
checks, never an idle watcher. A running pass reads as **Downloading metadata
N / ~M**: provider pacing (for example One's 15 requests/minute) is presented as
progress, not as a separate "waiting" state, so a paced sync is never mislabelled
as stalled. Safe errors and storage-capacity pauses remain visible. A short
Inbox-only arrival check cannot mark the whole mailbox's initial download complete.

Migration 069 adds `inbox_metadata_sync`, containing only safe state, phase, total
and timestamps. Existing cursors and metadata remain authoritative. On restart,
unfinished work is shown as pending, not a permanently stuck running spinner.
Status reads are scoped to the principal's account and allowed inboxes; the UI
endpoint does not fetch provider mail or reveal credentials.
