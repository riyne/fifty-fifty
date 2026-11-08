// Errors the client raises. The api's error body is {code, message} (components/schemas/Error
// in api/openapi.yaml). The text of a response that is not that shape is never shown: it could
// be an HTML page from a proxy, and a screen has no use for it.

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  /** Seconds from a Retry-After header, or null when there was none. */
  readonly retryAfterSeconds: number | null;

  constructor(status: number, code: string, message: string, retryAfterSeconds: number | null = null) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

/** The request did not produce a response (no network, a refused connection, a timeout). */
export class NetworkError extends Error {
  constructor(cause?: unknown) {
    super("could not reach the server");
    this.name = "NetworkError";
    this.cause = cause;
  }
}

function parseRetryAfter(value: string | null): number | null {
  if (value === null || !/^\d{1,9}$/.test(value.trim())) return null;
  return Number(value.trim());
}

function isErrorBody(body: unknown): body is { code: string; message: string } {
  return (
    typeof body === "object" &&
    body !== null &&
    typeof (body as { code?: unknown }).code === "string" &&
    typeof (body as { message?: unknown }).message === "string"
  );
}

/**
 * A sentence for a person. `byStatus` replaces the text for one HTTP status where a screen
 * knows better than the api's message (for example a join code that did not work). The api's
 * own 4xx messages are written to be shown; 5xx and unknown failures are not.
 */
export function describeError(error: unknown, byStatus: Partial<Record<number, string>> = {}): string {
  if (error instanceof NetworkError) return "Can't reach the server. Is the api running?";
  if (error instanceof ApiError) {
    const custom = byStatus[error.status];
    if (custom !== undefined) return custom;
    if (error.status === 401) return "Your session ended. Sign in again.";
    // the receipt upload's own limits (api/openapi.yaml): a daily quota whose Retry-After can be
    // hours away, and a cap on uploads processed at once
    if (error.code === "quota_exceeded") return "You've used today's receipt uploads. The limit resets at midnight UTC.";
    if (error.status === 503 && error.code === "busy") {
      return error.retryAfterSeconds === null
        ? "The server is busy with other uploads. Try again shortly."
        : `The server is busy with other uploads. Try again in ${error.retryAfterSeconds} s.`;
    }
    if (error.status === 429) {
      return error.retryAfterSeconds === null
        ? "Too many requests. Try again shortly."
        : `Too many requests. Try again in ${error.retryAfterSeconds} s.`;
    }
    if (error.status >= 500) return "The server had a problem. Try again.";
    return error.message;
  }
  return "Something went wrong.";
}

/** Builds the error for a non-2xx response; `body` is whatever the response parsed to. */
export function apiErrorFrom(response: Response, body: unknown): ApiError {
  const retryAfter = parseRetryAfter(response.headers.get("retry-after"));
  if (isErrorBody(body)) {
    return new ApiError(response.status, body.code, body.message, retryAfter);
  }
  return new ApiError(response.status, "unknown", `unexpected response (HTTP ${response.status})`, retryAfter);
}
