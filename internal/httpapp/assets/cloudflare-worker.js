// Cloudflare Worker: forward Email Routing messages to Gatehouse Email.
//
// This file is a template. Gatehouse Email substitutes the two placeholder
// values below when it generates your ready-to-paste Worker code in the Admin
// UI. You do not normally edit this file by hand.
const WEBHOOK_URL = "__GATEHOUSE_WEBHOOK_URL__";
const SECRET = "__GATEHOUSE_WEBHOOK_SECRET__";

export default {
  async email(message, env, ctx) {
    const resp = await fetch(WEBHOOK_URL, {
      method: "POST",
      headers: {
        "Content-Type": "message/rfc822",
        "Authorization": "Bearer " + SECRET,
        // Envelope metadata; the recipient selects the domain and its secret.
        "X-Gatehouse-Recipient": message.to,
        "X-Gatehouse-Envelope-From": message.from,
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
