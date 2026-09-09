// Cloudflare Worker: forward Email Routing messages to Gatehouse Email.
//
// Deployment steps and dashboard navigation are in docs/CLOUDFLARE_INBOUND.md.
//
// The Worker streams the raw MIME to Gatehouse without buffering it. Set
// WEBHOOK_URL to your instance's Cloudflare ingest endpoint and make SECRET
// match the webhook secret on the domain's receive path. Prefer binding
// GATEHOUSE_WEBHOOK_SECRET as a Worker secret; the SECRET constant is a
// fallback for quick testing.
const WEBHOOK_URL = "https://mail.example.com/internal/ingest/cloudflare";
const SECRET = "replace-with-your-gatehouse-webhook-secret";

export default {
  async email(message, env, ctx) {
    const secret = (env && env.GATEHOUSE_WEBHOOK_SECRET) || SECRET;

    const resp = await fetch(WEBHOOK_URL, {
      method: "POST",
      headers: {
        "Content-Type": "message/rfc822",
        "Authorization": "Bearer " + secret,
        // Envelope metadata; the recipient selects the domain and its secret.
        "X-Gatehouse-Recipient": message.to,
        "X-Gatehouse-Envelope-From": message.from,
        // Optional stable delivery id; when absent the server derives one from
        // a hash of the raw MIME.
        // "X-Gatehouse-Delivery-ID": "",
      },
      // message.raw is a ReadableStream and is forwarded as-is.
      body: message.raw,
    });

    if (!resp.ok) {
      const detail = await resp.text();
      // Throwing surfaces the failure in Worker logs and lets Cloudflare retry.
      // Gatehouse returns 4xx for permanent rejections and 5xx for retryable
      // failures; inspect `resp.status` to distinguish them.
      throw new Error("gatehouse forward failed: " + resp.status + " " + detail);
    }
  },
};
