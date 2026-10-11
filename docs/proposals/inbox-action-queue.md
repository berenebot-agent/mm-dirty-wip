# Durable inbox action queue

Approved by Ben on 2026-10-11: one action submission core for domain and remote
inboxes, dedicated in-process action worker, async default and `?wait=true`
matching send. The header status flag is the queue entry point across pages;
pending actions and failures are account-wide but mailbox-role scoped. Retry and
dismiss are supported. Purge is queued too, with existing Assistant/Owner rules.

Confirmed provider locators must never be overwritten with optimistic folder
intent. The UI overlays pending changes on confirmed rows; failed jobs stop
contributing that overlay. Jobs are serialized per message, rather than dropping
an earlier unrelated mutation when a later action arrives. Interrupted remote
moves/purges are ambiguous and require verification, not blind retry.

The queue remains SQLite-backed, inside the existing process/container. It uses
an immediate wake and a periodic backstop. Completed records expire after seven
days; failures remain visible until dismissed. REST mutations expose an action
handle and polling through the existing single-message GET route.
