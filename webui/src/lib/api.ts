import type {
  ApiErrorBody,
  CreateKeyRequest,
  CreateKeyResponse,
  KeysResponse,
  RevokeKeyResponse,
  UsageResponse,
} from "./types";

export class ApiError extends Error {
  readonly status: number;
  readonly type: string;
  readonly code: string;

  constructor(status: number, message: string, type: string, code: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.type = type;
    this.code = code;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      headers: { Accept: "application/json", ...init?.headers },
      ...init,
    });
  } catch {
    throw new ApiError(0, "Network error: could not reach the server", "network", "network_error");
  }
  if (!response.ok) {
    let message = `Request failed with status ${response.status}`;
    let type = "api_error";
    let code = "unknown";
    try {
      const body = (await response.json()) as ApiErrorBody;
      if (body?.error?.message) {
        ({ message, type, code } = body.error);
      }
    } catch {
      // non-JSON error body; keep the status-based message
    }
    throw new ApiError(response.status, message, type, code);
  }
  return (await response.json()) as T;
}

export function listKeys(): Promise<KeysResponse> {
  return request<KeysResponse>("api/keys");
}

export function createKey(body: CreateKeyRequest): Promise<CreateKeyResponse> {
  return request<CreateKeyResponse>("api/keys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export function revokeKey(id: string): Promise<RevokeKeyResponse> {
  return request<RevokeKeyResponse>(`api/keys/${encodeURIComponent(id)}/revoke`, {
    method: "POST",
  });
}

export function getUsage(): Promise<UsageResponse> {
  return request<UsageResponse>("api/usage");
}
