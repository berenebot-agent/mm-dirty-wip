// Cloudflare Worker: forward Email Routing messages to Open Agent Inbox.
//
// Deployment steps and dashboard navigation are in docs/CLOUDFLARE_INBOUND.md.
//
// Update these two constants:
//   WEBHOOK_URL - your instance's Cloudflare ingest endpoint
//   SECRET       - must match CLOUDFLARE_WEBHOOK_SECRET
const WEBHOOK_URL = "https://mail.example.com/internal/ingest/cloudflare";
const SECRET = "replace-with-your-cloudeflare-webhook-secret";

export default {
  async email(message, env, ctx) {
    // message.raw is an async iterable; Buffer.from with the full string is
    // fine for typical mail sizes. The server rejects oversized payloads.
    const raw = Buffer.from(await message.raw, "utf8");

    const payload = {
      recipient: message.to,
      envelope_from: message.from,
      raw_mime_b64: raw.toString("base64"),
      delivery_id: "", // leave empty; server hashes the MIME for dedup
      received_at: new Date().toISOString(),
    };

    const resp = await fetch(WEBHOOK_URL, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: "Bearer " + SECRET,
      },
      body: JSON.stringify(payload),
    });

    if (!resp.ok) {
      // Email Routing drops email silently on error; log for triage and
      // consider a durable queue (Workers Queue / KV retry) if you need
      // at-least-once delivery.
      console.error("gatehouse forward failed", resp.status, await resp.text());
    }
  },
};
