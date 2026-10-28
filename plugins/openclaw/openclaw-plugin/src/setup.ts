/**
 * Mammoth Mail setup adapter.
 *
 * Two onboarding paths:
 *   1. One-time setup code (recommended): Mammoth Mail shows
 *      `openclaw channels add --channel mammothmail --code <setup-url>`. The URL
 *      origin is the Mammoth Mail base URL and the fragment carries the code; the
 *      adapter redeems it at POST /relay/enroll and stores the returned secret.
 *   2. Manual config: baseUrl + gatewayId + secret are supplied directly.
 *
 * The code is an authority, not an address: it does not carry the URL, so the
 * setup URL (or an explicit --base-url) is what tells the plugin where
 * Mammoth Mail lives.
 */
import { createRequire } from "node:module";
import { defineChannelSetupContract } from "openclaw/plugin-sdk/channel-setup";
import { DEFAULT_ACCOUNT_ID, normalizeAccountId, patchScopedAccountConfig } from "openclaw/plugin-sdk/setup";
import { formatErrorMessage } from "openclaw/plugin-sdk/error-runtime";
import { normalizeBaseUrl } from "./accounts.js";
import type { CoreConfig } from "./types.js";

const require = createRequire(import.meta.url);

export type MammothMailSetupInput = {
  name?: string;
  code?: string;
  baseUrl?: string;
  gatewayId?: string;
  secret?: string;
  allowFrom?: string[];
};

type ClaimResult = {
  secret: string;
  gatewayId: string;
  name?: string;
  kind?: string;
};

/** Parses a setup URL or bare code into a base URL and code. */
export function parseSetupCode(params: { code: string; baseUrl?: string }): {
  baseUrl: string;
  code: string;
} {
  const raw = params.code.trim();
  if (!raw) {
    throw new Error("Mammoth Mail setup code is required");
  }
  if (/^[a-z][a-z\d+.-]*:\/\//iu.test(raw)) {
    const url = new URL(raw);
    if (url.protocol !== "http:" && url.protocol !== "https:") {
      throw new Error("Mammoth Mail setup URL must use http(s)");
    }
    const code = url.hash.replace(/^#/, "").trim();
    if (!code) {
      throw new Error("Mammoth Mail setup URL is missing its #code fragment");
    }
    url.hash = "";
    const baseUrl = normalizeBaseUrl(url.toString());
    if (!baseUrl) {
      throw new Error("Mammoth Mail setup URL is invalid");
    }
    return { baseUrl, code };
  }
  const baseUrl = normalizeBaseUrl(params.baseUrl);
  if (!baseUrl) {
    throw new Error("A bare Mammoth Mail setup code requires --base-url");
  }
  return { baseUrl, code: raw };
}

/** Generates a stable, host-unique gateway id for a new connector. */
export function generateGatewayId(): string {
  const crypto = require("node:crypto") as typeof import("node:crypto");
  return `gw-oc-${crypto.randomBytes(8).toString("hex")}`;
}

/** Redeems a one-time code for relay credentials. */
export async function claimMammothMailSetupCode(params: {
  baseUrl: string;
  code: string;
  gatewayId?: string;
  fetchImpl?: typeof fetch;
}): Promise<ClaimResult> {
  const gatewayId = params.gatewayId?.trim() || generateGatewayId();
  const endpoint = `${params.baseUrl.replace(/\/+$/, "")}/relay/enroll`;
  const fetchImpl = params.fetchImpl ?? fetch;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 30_000);
  timer.unref?.();
  try {
    const response = await fetchImpl(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enrollmentToken: params.code, gatewayId }),
      signal: controller.signal,
    });
    if (!response.ok) {
      throw new Error(
        response.status === 403
          ? "Mammoth Mail setup code is invalid, expired or already used"
          : `Mammoth Mail enrollment failed with HTTP ${response.status}`,
      );
    }
    const body = (await response.json()) as {
      secret?: string;
      gatewayId?: string;
      name?: string;
      kind?: string;
    };
    if (!body.secret) {
      throw new Error("Mammoth Mail enrollment response was missing a secret");
    }
    return {
      secret: body.secret,
      gatewayId: body.gatewayId || gatewayId,
      name: body.name,
      kind: body.kind,
    };
  } finally {
    clearTimeout(timer);
  }
}

export const mammothMailSetupAdapter = {
  resolveAccountId: ({ accountId }: { accountId?: string }) =>
    normalizeAccountId(accountId || DEFAULT_ACCOUNT_ID),
  prepareAccountConfigInput: async ({
    cfg,
    input,
  }: {
    cfg: CoreConfig;
    accountId: string;
    input: MammothMailSetupInput;
  }): Promise<MammothMailSetupInput> => {
    if (!input.code?.trim()) {
      return input;
    }
    if (input.secret?.trim() || input.gatewayId?.trim()) {
      throw new Error("Mammoth Mail --code cannot be combined with --secret or --gateway-id");
    }
    const parsed = parseSetupCode({ code: input.code, baseUrl: input.baseUrl });
    const claim = await claimMammothMailSetupCode({ baseUrl: parsed.baseUrl, code: parsed.code });
    const { code: _code, ...rest } = input;
    return {
      ...rest,
      baseUrl: parsed.baseUrl,
      gatewayId: claim.gatewayId,
      secret: claim.secret,
    };
  },
  validateInput: ({ input }: { cfg: CoreConfig; accountId: string; input: MammothMailSetupInput }) => {
    if (input.code?.trim()) {
      try {
        parseSetupCode({ code: input.code, baseUrl: input.baseUrl });
      } catch (error) {
        return formatErrorMessage(error);
      }
      return null;
    }
    if (!normalizeBaseUrl(input.baseUrl)) {
      return "Mammoth Mail requires --base-url";
    }
    if (!input.gatewayId?.trim()) {
      return "Mammoth Mail requires --gateway-id";
    }
    if (!input.secret?.trim()) {
      return "Mammoth Mail requires --secret";
    }
    return null;
  },
  applyAccountConfig: ({
    cfg,
    accountId,
    input,
  }: {
    cfg: CoreConfig;
    accountId: string;
    input: MammothMailSetupInput;
  }) =>
    patchScopedAccountConfig({
      cfg: cfg as never,
      channelKey: "mammothmail",
      accountId,
      ensureChannelEnabled: true,
      ensureAccountEnabled: true,
      clearFields: input.code ? ["code"] : [],
      patch: {
        ...(normalizeBaseUrl(input.baseUrl) ? { baseUrl: normalizeBaseUrl(input.baseUrl) } : {}),
        ...(input.gatewayId?.trim() ? { gatewayId: input.gatewayId.trim() } : {}),
        ...(input.secret?.trim() ? { secret: input.secret.trim() } : {}),
        ...(input.name?.trim() ? { name: input.name.trim() } : {}),
      },
    }),
};

export const mammothMailSetupContract = defineChannelSetupContract({
  fields: {
    code: {
      kind: "string",
      sensitive: true,
      cli: { flags: "--code <url-or-code>", description: "Mammoth Mail one-time setup URL or code" },
    },
    baseUrl: {
      kind: "string",
      cli: { flags: "--base-url <url>", description: "Mammoth Mail base URL" },
    },
    gatewayId: {
      kind: "string",
      cli: { flags: "--gateway-id <id>", description: "Mammoth Mail connector gateway id" },
    },
    secret: {
      kind: "string",
      sensitive: true,
      cli: { flags: "--secret <secret>", description: "Mammoth Mail relay secret" },
    },
  },
  legacyAdapter: mammothMailSetupAdapter,
});