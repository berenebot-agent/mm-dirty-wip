/**
 * Shared types for the Mammoth Mail OpenClaw channel plugin.
 *
 * Mammoth Mail is an email front end: this plugin dials OUT to the Mammoth Mail
 * relay with an authenticated WebSocket, receives inbound email events and
 * sends replies back over the same socket. No inbound port is required on the
 * OpenClaw host.
 */

export type MammothMailChannelConfig = {
  enabled?: boolean;
  name?: string;
  baseUrl?: string;
  gatewayId?: string;
  secret?: string;
  allowFrom?: string[];
  dmPolicy?: string;
  agentId?: string;
};

export type CoreConfig = {
  channels?: Record<string, unknown>;
  [key: string]: unknown;
};

export type ResolvedMammothMailAccount = {
  accountId: string;
  name?: string;
  enabled: boolean;
  configured: boolean;
  baseUrl: string;
  gatewayId: string;
  secret: string;
  allowFrom: string[];
  dmPolicy: string;
  agentId?: string;
  tokenStatus: "available" | "missing";
};

/** One inbound email event as delivered by Mammoth Mail on the relay socket. */
export type MammothMailInboundEvent = {
  text: string;
  message_type?: string;
  user_id: string;
  user_name?: string;
  message_id: string;
  source: {
    platform: "email" | string;
    chat_id: string;
    chat_type?: string;
    chat_name?: string;
    user_id?: string;
    user_name?: string;
    thread_id?: string;
    message_id?: string;
  };
  metadata?: Record<string, unknown>;
  provenance?: Record<string, unknown>;
  timestamp?: string;
};

export type RelayFrame = {
  type?: string;
  bufferId?: string;
  requestId?: string;
  event?: MammothMailInboundEvent;
  action?: {
    op?: string;
    chat_id?: string;
    thread_id?: string;
    reply_to_message_id?: string;
    content?: string;
    text?: string;
  };
  result?: {
    success?: boolean;
    message_id?: string;
    error?: string;
  };
};
