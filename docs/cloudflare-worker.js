// Cloudflare Worker: forward Email Routing messages to Open Agent Inbox.
//
// Deployment steps and dashboard navigation are in docs/CLOUDFLARE_INBOUND.md.
//
// Update these two constants:
//   WEBHOOK_URL - your instance's Cloudflare ingest endpoint
//   SECRET       - must match CLOUDFLARE_WEBHOOK_SECRET
const WEBHOOK_URL = "https://mail.example.com/internal/ingest/cloudflare";
const SECRET = "replace-with-your-cloudeflare-webhook-secret";

// Helper function to safely convert ArrayBuffer to Base64 in V8/Workers
function arrayBufferToBase64(buffer) {
  let binary = "";
  const bytes = new Uint8Array(buffer);
  const len = bytes.byteLength;
  for (let i = 0; i < len; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}

export default {
  async email(message, env, ctx) {
    // Read the message.raw ReadableStream as an ArrayBuffer
    const rawBuffer = await new Response(message.raw).arrayBuffer();
    const raw_mime_b64 = arrayBufferToBase64(rawBuffer);

    const payload = {
      recipient: message.to,
      envelope_from: message.from,
      raw_mime_b64: raw_mime_b64,
      delivery_id: "", // leave empty; server hashes the MIME for dedup
      received_at: new Date().toISOString(),
    };

    const resp = await fetch(WEBHOOK_URL, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Authorization": "Bearer " + SECRET,
      },
      body: JSON.stringify(payload),
    });

    if (!resp.ok) {
      console.error("gatehouse forward failed", resp.status, await resp.text());
    }
  },
};