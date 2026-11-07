/**
 * @jest-environment node
 */
import { createApi, unwrap } from "./client";
import { ApiError, NetworkError } from "./errors";

const BASE = "http://api.test";

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

/** An api client whose network is a function; `seen` records the requests it got. */
function setup(answer: (request: Request) => Response | Promise<Response>, token: string | null = "tok-1") {
  const seen: Request[] = [];
  let unauthorized = 0;
  const api = createApi({
    baseUrl: BASE,
    getToken: () => token,
    onUnauthorized: () => {
      unauthorized++;
    },
    fetch: async (request) => {
      seen.push(request.clone());
      return answer(request);
    },
  });
  return { api, seen, unauthorizedCalls: () => unauthorized };
}

describe("request shape", () => {
  it("sends the bearer token and calls the path under the base url", async () => {
    const { api, seen } = setup(() => json(200, { id: "u1", email: "a@example.com", display_name: "Alice" }));
    const me = await unwrap(api.GET("/v1/me"));
    expect(me.display_name).toBe("Alice");
    expect(seen).toHaveLength(1);
    expect(seen[0].url).toBe(`${BASE}/v1/me`);
    expect(seen[0].headers.get("authorization")).toBe("Bearer tok-1");
  });

  it("sends no Authorization header when logged out", async () => {
    const { api, seen } = setup(() => json(200, { status: "ok" }), null);
    await unwrap(api.GET("/healthz"));
    expect(seen[0].headers.has("authorization")).toBe(false);
  });

  it("sends the Idempotency-Key and the JSON body of a mutating call", async () => {
    const { api, seen } = setup(() =>
      json(201, { id: "g1", name: "Trip", currency: "CAD", role: "member", created_at: "2026-11-07T00:00:00Z" }),
    );
    const group = await unwrap(
      api.POST("/v1/join", { params: { header: { "Idempotency-Key": "key-1" } }, body: { code: "ABCD-EFGH-JKMN" } }),
    );
    expect(group.id).toBe("g1");
    expect(seen[0].method).toBe("POST");
    expect(seen[0].headers.get("idempotency-key")).toBe("key-1");
    expect(await seen[0].json()).toEqual({ code: "ABCD-EFGH-JKMN" });
  });

  it("puts path parameters into the url", async () => {
    const { api, seen } = setup(() => json(404, { code: "not_found", message: "group not found" }));
    await expect(unwrap(api.GET("/v1/groups/{id}", { params: { path: { id: "abc" } } }))).rejects.toBeInstanceOf(ApiError);
    expect(seen[0].url).toBe(`${BASE}/v1/groups/abc`);
  });
});

describe("the contract is enforced when the code is compiled", () => {
  // These calls are never made (the function is not called); `npm run typecheck` is the test.
  // If the generated types stopped requiring the key, an unused @ts-expect-error would fail it.
  it("is checked by tsc, not at runtime", () => {
    const never = () => {
      const { api } = setup(() => json(200, {}));
      // @ts-expect-error Idempotency-Key is a required header parameter of POST /v1/join
      void api.POST("/v1/join", { body: { code: "x" } });
      // @ts-expect-error the body is required
      void api.POST("/v1/join", { params: { header: { "Idempotency-Key": "k" } } });
      // @ts-expect-error a required body field is missing
      void api.POST("/v1/join", { params: { header: { "Idempotency-Key": "k" } }, body: {} });
      // @ts-expect-error a body field of the wrong type
      void api.POST("/v1/join", { params: { header: { "Idempotency-Key": "k" } }, body: { code: 12 } });
      // NOT enforced: an extra field in the body (additionalProperties: false) compiles, because
      // excess-property checks do not survive the client's generic parameter types. The api
      // rejects it with a 400; the tests of the api, not these, cover that.
      // @ts-expect-error there is no such path
      void api.GET("/v1/nothing");
    };
    expect(typeof never).toBe("function");
  });
});

describe("unwrap", () => {
  it("turns an error body into an ApiError with status and code", async () => {
    const { api } = setup(() => json(409, { code: "invalid_state", message: "expense is not claimable" }));
    const failure = unwrap(api.GET("/v1/me"));
    await expect(failure).rejects.toMatchObject({
      name: "ApiError",
      status: 409,
      code: "invalid_state",
      message: "expense is not claimable",
      retryAfterSeconds: null,
    });
  });

  it("reads Retry-After in seconds and ignores a value that is not", async () => {
    const a = setup(() => json(429, { code: "rate_limited", message: "slow down" }, { "retry-after": "7" }));
    await expect(unwrap(a.api.GET("/v1/me"))).rejects.toMatchObject({ code: "rate_limited", retryAfterSeconds: 7 });
    const b = setup(() => json(429, { code: "rate_limited", message: "slow down" }, { "retry-after": "Wed, 21 Oct" }));
    await expect(unwrap(b.api.GET("/v1/me"))).rejects.toMatchObject({ retryAfterSeconds: null });
  });

  it("does not show the text of a response that is not an api error", async () => {
    const { api } = setup(() => new Response("<html>502 Bad Gateway from some proxy</html>", { status: 502 }));
    const err = await unwrap(api.GET("/v1/me")).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("unknown");
    expect((err as ApiError).status).toBe(502);
    expect((err as ApiError).message).not.toContain("proxy");
  });

  it("resolves a 204 to undefined", async () => {
    const { api } = setup(() => new Response(null, { status: 204 }));
    await expect(
      unwrap(
        api.DELETE("/v1/groups/{id}/join-codes/{codeId}", {
          params: { path: { id: "g", codeId: "c" } },
        }),
      ),
    ).resolves.toBeUndefined();
  });

  it("raises NetworkError when there is no response, and keeps the cause", async () => {
    const cause = new TypeError("Network request failed");
    const { api } = setup(() => {
      throw cause;
    });
    const err = await unwrap(api.GET("/v1/me")).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(NetworkError);
    expect((err as NetworkError).cause).toBe(cause);
  });
});

describe("401", () => {
  it("calls onUnauthorized once and still raises the error", async () => {
    const { api, unauthorizedCalls } = setup(() => json(401, { code: "unauthorized", message: "invalid token" }));
    await expect(unwrap(api.GET("/v1/me"))).rejects.toMatchObject({ status: 401, code: "unauthorized" });
    expect(unauthorizedCalls()).toBe(1);
  });

  it("does not call it for other statuses", async () => {
    const { api, unauthorizedCalls } = setup(() => json(403, { code: "forbidden", message: "no" }));
    await expect(unwrap(api.GET("/v1/me"))).rejects.toMatchObject({ status: 403 });
    expect(unauthorizedCalls()).toBe(0);
  });
});
