/**
 * Account resolution for the MailMoose channel.
 *
 * One MailMoose connector is normally the only account, but the helpers keep
 * the same account-scoped shape as every bundled channel so `channels
 * mailmoose accounts.<id>` works if an operator runs several connectors.
 */
import {
  createAccountListHelpers,
  hasConfiguredAccountValue,
} from "openclaw/plugin-sdk/account-helpers";
import { DEFAULT_ACCOUNT_ID, normalizeAccountId } from "openclaw/plugin-sdk/account-id";
import { normalizeOptionalString } from "openclaw/plugin-sdk/string-coerce-runtime";
import type {
  CoreConfig,
  MailMooseChannelConfig,
  ResolvedMailMooseAccount,
} from "./types.js";

const { listAccountIds, resolveDefaultAccountId, resolveAccountConfig } =
  createAccountListHelpers<MailMooseChannelConfig>("mailmoose", {
    normalizeAccountId,
    omitKeys: ["defaultAccount"],
    // Setup writes the connector at the channel root for the default account
    // (like the other bundled channels), so root values must count as the
    // implicit default account.
    hasImplicitDefaultAccount: (cfg) => {
      const channel = (cfg as CoreConfig).channels?.mailmoose as
        | MailMooseChannelConfig
        | undefined;
      return Boolean(
        channel?.baseUrl?.trim() && channel.gatewayId?.trim() && channel.secret?.trim(),
      );
    },
  });

export { DEFAULT_ACCOUNT_ID, listAccountIds, resolveDefaultAccountId };

export function listEnabledMailMooseAccounts(cfg: CoreConfig): string[] {
  return listAccountIds(cfg as never).filter((accountId) => {
    const account = resolveMailMooseAccount({ cfg, accountId });
    return account.enabled && account.configured;
  });
}

export type ResolveMailMooseAccountParams = {
  cfg: CoreConfig;
  accountId?: string | null;
};

/** Resolves one account's configuration into its runtime shape. */
export function resolveMailMooseAccount(
  params: ResolveMailMooseAccountParams,
): ResolvedMailMooseAccount {
  const accountId = normalizeAccountId(params.accountId ?? resolveDefaultAccountId(params.cfg as never));
  const config = resolveAccountConfig(params.cfg as never, accountId) ?? {};
  const baseUrl = normalizeBaseUrl(config.baseUrl);
  const gatewayId = normalizeOptionalString(config.gatewayId) ?? "";
  const secret = normalizeOptionalString(config.secret) ?? "";
  const configured = Boolean(baseUrl && gatewayId && secret);
  return {
    accountId,
    name: normalizeOptionalString(config.name),
    enabled: config.enabled !== false,
    configured,
    baseUrl,
    gatewayId,
    secret,
    allowFrom: normalizeAllowFrom(config.allowFrom),
    dmPolicy: normalizeOptionalString(config.dmPolicy) ?? "allowlist",
    agentId: normalizeOptionalString(config.agentId),
    tokenStatus: secret ? "available" : "missing",
  };
}

export function resolveMailMooseAccountConfig(
  cfg: CoreConfig,
  accountId: string,
): MailMooseChannelConfig {
  return resolveAccountConfig(cfg as never, normalizeAccountId(accountId)) ?? {};
}

export function isMailMooseAccountCurrent(params: {
  cfg: CoreConfig;
  account: ResolvedMailMooseAccount;
}): boolean {
  const current = resolveMailMooseAccount({
    cfg: params.cfg,
    accountId: params.account.accountId,
  });
  return (
    current.baseUrl === params.account.baseUrl &&
    current.gatewayId === params.account.gatewayId &&
    current.secret === params.account.secret
  );
}

/** Normalizes an http(s) base URL, dropping trailing slashes. */
export function normalizeBaseUrl(value: unknown): string {
  const raw = normalizeOptionalString(value);
  if (!raw) {
    return "";
  }
  try {
    const parsed = new URL(raw);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
      return "";
    }
    if (parsed.username || parsed.password || parsed.search || parsed.hash) {
      return "";
    }
    return (parsed.origin + parsed.pathname).replace(/\/+$/, "");
  } catch {
    return "";
  }
}

function normalizeAllowFrom(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value
    .map((entry) => normalizeOptionalString(entry))
    .filter((entry): entry is string => Boolean(entry));
}

export { hasConfiguredAccountValue };
