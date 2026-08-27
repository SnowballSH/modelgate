export interface ApiKey {
  id: string;
  prefix: string;
  label: string;
  models: string[] | null;
  quota_usd: number | null;
  expires_at: string | null;
  revoked_at: string | null;
  last_used_at: string | null;
  created_at: string;
  created_by: string;
  month_spend_usd: number;
}

export interface KeysResponse {
  keys: ApiKey[];
}

export interface CreateKeyRequest {
  label: string;
  models?: string[];
  quota_usd?: number;
  expires_at?: string;
}

export interface CreateKeyResponse {
  key: ApiKey;
  full_key: string;
}

export interface RevokeKeyResponse {
  key: ApiKey;
}

export interface UsageKey {
  id: string;
  label: string;
  spend_usd: number;
  quota_usd: number | null;
}

export interface UsageResponse {
  month: string;
  budget_usd: number;
  spend_usd: number;
  keys: UsageKey[];
}

export interface ConfiguredModel {
  id: string;
  provider: string;
}

export interface ModelsResponse {
  models: ConfiguredModel[];
}

export interface ApiErrorBody {
  error: {
    message: string;
    type: string;
    code: string;
  };
}

export type KeyState = "active" | "expired" | "revoked";

export function keyState(key: ApiKey, now: Date = new Date()): KeyState {
  if (key.revoked_at !== null) return "revoked";
  if (key.expires_at !== null && new Date(key.expires_at) <= now) {
    return "expired";
  }
  return "active";
}
