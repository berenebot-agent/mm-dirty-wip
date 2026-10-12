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
folder's cache count). Gmail/One display **N indexed**: Gmail's profile total
includes Spam/Trash while the backfill list excludes them, and its list count is
an estimate, so neither is a trustworthy denominator for the current scan. Totals
are snapshots and can change as provider mail changes; an indexed count exceeding
the total suppresses that denominator rather than manufacturing a percentage.

Queued/running status reflects this process's actual workers and short metadata
checks, never an idle watcher. Between backfill batches, the row remains waiting
rather than claiming an active download. Safe errors and storage-capacity pauses
remain visible. A short Inbox-only arrival check cannot mark the whole mailbox's
initial download complete.

Migration 069 adds `inbox_metadata_sync`, containing only safe state, phase, total
and timestamps. Existing cursors and metadata remain authoritative. On restart,
unfinished work is shown as waiting, not a permanently stuck running spinner.
Status reads are scoped to the principal's account and allowed inboxes; the UI
endpoint does not fetch provider mail or reveal credentials.
