import createClient from "openapi-fetch";

import { apiErrorFrom, NetworkError } from "./errors";
import type { paths } from "./schema";

export interface ApiOptions {
  baseUrl: string;
  /** The ID token to send as the bearer, or null when logged out. Called for every request. */
  getToken: () => Promise<string | null> | string | null;
  /** Called when the api answers 401 (missing, invalid or expired token); the app goes back to login. */
  onUnauthorized?: () => void;
  /** For tests. Defaults to the global fetch. */
  fetch?: (request: Request) => Promise<Response>;
}

/**
 * The api client. Every path, parameter, body and response type comes from api/openapi.yaml
 * through the generated schema.d.ts, so a call the contract does not allow does not compile.
 * In particular Idempotency-Key is a required header parameter on every operation that needs
 * one, so a mutating call without a key is a type error.
 */
export function createApi(options: ApiOptions) {
  const client = createClient<paths>({ baseUrl: options.baseUrl, fetch: options.fetch });
  client.use({
    async onRequest({ request }) {
      const token = await options.getToken();
      if (token) request.headers.set("Authorization", `Bearer ${token}`);
      return request;
    },
    onResponse({ response }) {
      if (response.status === 401) options.onUnauthorized?.();
    },
  });
  return client;
}

export type Api = ReturnType<typeof createApi>;

type Settled<D> = { data?: D; error?: unknown; response: Response };

/**
 * Turns the {data, error, response} that the client returns into "the data, or a throw":
 * ApiError for a non-2xx answer, NetworkError when there was no answer. A 2xx with no body
 * (204) resolves to undefined.
 */
export async function unwrap<D>(request: Promise<Settled<D>>): Promise<[D] extends [never] ? void : D> {
  let settled: Settled<D>;
  try {
    settled = await request;
  } catch (cause) {
    throw new NetworkError(cause);
  }
  if (!settled.response.ok) throw apiErrorFrom(settled.response, settled.error);
  return settled.data as [D] extends [never] ? void : D;
}
