# Local OpenClaw test harness for the MailMoose connector

Scratch space for exercising the OpenClaw plugin in `../openclaw-plugin` against
a real MailMoose instance. It is **not part of the shipped product** — the
connector itself lives in `../openclaw-plugin` and the MailMoose side in
`internal/{store,httpapp,hermesrelay}`.

This exists because the plugin's only real test surface is a live Gateway: the
unit tests cover target grammar, token format and setup-code claim, but nothing
covers "openclaw connects, receives an email, and replies on the thread".

## What you need

1. A MailMoose instance you do not mind using for testing (production is fine —
   the test uses its own inboxes).
2. An OpenClaw connector created in the MailMoose UI for the agent inbox, which
   mints a one-time setup code.
3. An OpenAI-compatible LLM endpoint for the Gateway's agent turns.

## Bring it up

```bash
cd plugins/openclaw/openclaw-plugin && npm ci && npm run build     # plugin must be built first
cd ../local-test
cp .env.example .env        # fill in the provider key
docker compose up -d
docker compose logs -f
```

Then write `state/openclaw.json` (mode 600 — it ends up holding the relay
secret in plaintext). Minimum shape:

```jsonc
{
  "models": { "mode": "merge", "providers": {
    "tiller": { "baseUrl": "<llm-base>/v1", "apiKey": "$TILLER_API_KEY",
                "auth": "api-key", "api": "openai-completions",
                "models": [{ "id": "<model-id>" }] } } },
  "agents": { "defaults": { "model": "tiller/<model-id>", "workspace": "/state/workspace" } },
  "gateway": { "mode": "local", "bind": "loopback", "port": 18789, "auth": { "mode": "none" } },
  "plugins": { "load": { "paths": ["/plugin"] },
               "entries": { "mailmoose": { "enabled": true } } }
}
```

The `plugins.load.paths` entry is what makes the mounted plugin load; the
Gateway warns that a config-path plugin is untrusted (`trust.reason =
record-missing`) but loads it and logs `mailmoose` in the plugin list.

## Install the channel

In the container:

```bash
docker compose exec openclaw node openclaw.mjs plugins install -l /plugin --accept-capabilities --force
docker compose exec openclaw node openclaw.mjs channels add --channel mailmoose --code "<setup-url>"
```

`--force` is required for a local path install (the CLI refuses an unreviewed
source otherwise). The setup URL's **origin** must be a MailMoose address the
container can reach — if the instance's `BASE_URL` is not resolvable from here,
rewrite the origin and keep the `#code` fragment.

## Gotchas found the hard way

- **Host networking is required, not optional.** The Gateway needs the host's
  loopback for a local LLM endpoint, and a MailMoose throwaway container cannot
  use host networking at all (`:8082` is a fixed listener and the embedded MX
  edge owns `:2525`).
- **One Gateway per state directory.** `OPENCLAW_STATE_DIR` carries a lease, so
  a second Gateway on the same state dir exits with *"Another Gateway owner
  lease is still active for this state directory"*. Remove `state/state/` to
  reset, or use a separate state dir per instance.
- **The CLI is not on `PATH`.** Use `node openclaw.mjs …`, or the image's
  entrypoint form `node dist/index.js …`.
- **Node 24.16+ is required** by the plugin SDK — Node 22 fails at
  `openclaw`'s preinstall. The published images already ship Node 24.
- **`gateway.bind: loopback` is a warning, not an error** (`node-hosting-preconditions`).
