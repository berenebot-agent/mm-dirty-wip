# Hermes Relay setup guide preview

This is the wording shown after creating a Hermes Relay connector.

1. Add the generated relay block to your gateway's `.env` — usually `~/.hermes/.env`. Replace existing `GATEWAY_RELAY_*` lines; one relay connection owns the gateway.

   ```dotenv
   GATEWAY_RELAY_URL=https://mail.example.com
   GATEWAY_RELAY_ID=gw-...
   GATEWAY_RELAY_SECRET=...
   GATEWAY_RELAY_PLATFORMS=email
   GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true
   ```

   **Optional but recommended — reduce extra emails:**

   2. Set a home address alongside the relay lines in the same `.env`, so Hermes does not send correspondents a “No home channel is set” notice.

      ```dotenv
      EMAIL_HOME_ADDRESS=you@yourdomain.com
      EMAIL_HOME_ADDRESS_NAME=Your Agent Name
      ```

   3. Turn off memory and self-improvement notices globally in `~/.hermes/config.yaml` (merge into any existing `display` section):

      ```yaml
      display:
        memory_notifications: off
        suppress_warning_notifications: true
      ```

4. Restart the gateway to reload the configuration: `hermes gateway restart`.
