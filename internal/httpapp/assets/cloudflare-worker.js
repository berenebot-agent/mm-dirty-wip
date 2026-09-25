// Cloudflare Worker: forward Email Routing messages to MailMoose.
//
// This file is a template. MailMoose substitutes the two placeholder
// values below when it generates your ready-to-paste Worker code in the Admin
// UI. You do not normally edit this file by hand.
const WEBHOOK_URL = "__MAILMOOSE_WEBHOOK_URL__";
const SECRET = "__MAILMOOSE_WEBHOOK_SECRET__";

export default {
  async email(message, env, ctx) {
    const resp = await fetch(WEBHOOK_URL, {
      method: "POST",
      headers: {
        "Content-Type": "message/rfc822",
        "Authorization": "Bearer " + SECRET,
        // Envelope metadata; the recipient selects the domain and its secret.
        //
        // X-MailMoose-Envelope-From is caller-controlled METADATA, not
        // attestation: the bearer below authenticates the caller, but does not
        // prove the sender value is genuine. The approval workflow treats the
        // envelope sender as the approver identity, so a holder of this secret
        // can assert any sender. This is a known accepted risk (D058) — do not
        // rely on this header for authentication.
        "X-MailMoose-Recipient": message.to,
        "X-MailMoose-Envelope-From": message.from,
      },
      // message.raw is a ReadableStream and is forwarded as-is.
      body: message.raw,
    });

    if (!resp.ok) {
      const detail = await resp.text();
      // Throwing surfaces the failure in Worker logs and lets Cloudflare retry.
      // MailMoose returns 4xx for permanent rejections and 5xx for retryable
      // failures; inspect `resp.status` to distinguish them.
      throw new Error("mailmoose forward failed: " + resp.status + " " + detail);
    }
  },
};
