/**
 * @jest-environment node
 */
import { fakeJwt } from "./fakeJwt";
import { createSession, RefreshRejectedError, type RefreshStore, type Refresher, type Tokens, type TokenStore } from "./session";

// The refresh rules (M10.21). The session without `refreshing` is tested in session.test.ts.

const T0 = 1_000_000;
// Below, T0 + 45 is a token that signIn accepts (more than 30 s left) but that is inside the 60 s refresh margin.
const idToken = (tag: string, exp: number) => fakeJwt({ sub: tag, exp });

function fakeStore<T extends string>(initial: string | null = null) {
  const s = {
    value: initial as string | null,
    saves: [] as string[],
    clears: 0,
    failClear: false,
    async load() {
      return s.value;
    },
    async save(v: string) {
      s.saves.push(v);
      s.value = v;
    },
    async clear() {
      s.clears++;
      if (s.failClear) throw new Error("keychain");
      s.value = null;
    },
  };
  return s satisfies TokenStore & RefreshStore;
}

/** A refresher whose answer each test decides, with a count of calls and the tokens it was given. */
function fakeRefresher(answer: (rt: string, call: number) => Promise<Tokens>) {
  const calls: string[] = [];
  const refresh: Refresher = (rt) => {
    calls.push(rt);
    return answer(rt, calls.length);
  };
  return { refresh, calls };
}

function setup(opts: { id?: string | null; refresh?: string | null; answer?: (rt: string, call: number) => Promise<Tokens> } = {}) {
  let now = T0;
  const idStore = fakeStore(opts.id === undefined ? null : opts.id);
  const rtStore = fakeStore(opts.refresh === undefined ? null : opts.refresh);
  const refresher = fakeRefresher(opts.answer ?? (async (_rt, call) => ({ idToken: idToken(`r${call}`, now + 3600), refreshToken: `rt${call}` })));
  const session = createSession(idStore, () => now, { store: rtStore, refresh: refresher.refresh });
  return { session, idStore, rtStore, refresher, advance: (s: number) => (now += s), now: () => now };
}

const settle = () => new Promise<void>((r) => setImmediate(() => r())); // let fire-and-forget saves finish

describe("restore with a refresh token", () => {
  it("refreshes instead of signing out when the ID token has expired", async () => {
    const t = setup({ id: idToken("old", T0 - 5), refresh: "rt0" });
    await t.session.restore();
    expect(t.session.getState().status).toBe("signedIn");
    expect(t.refresher.calls).toEqual(["rt0"]);
    expect(t.session.getToken()).toBe(t.idStore.value); // the new token was saved
    await settle();
    expect(t.rtStore.value).toBe("rt1"); // the rotated one replaced the old
  });

  it("does not refresh when the ID token is still good", async () => {
    const t = setup({ id: idToken("a", T0 + 3600), refresh: "rt0" });
    await t.session.restore();
    expect(t.session.getState().status).toBe("signedIn");
    expect(t.refresher.calls).toEqual([]);
  });

  it("signs out and clears both items when the provider refuses the refresh token", async () => {
    const t = setup({ id: idToken("old", T0 - 5), refresh: "rt0", answer: async () => { throw new RefreshRejectedError("invalid_grant"); } });
    await t.session.restore();
    expect(t.session.getState().status).toBe("signedOut");
    expect(t.idStore.value).toBeNull();
    expect(t.rtStore.value).toBeNull();
  });

  it("stays signed in with the old token, and keeps both items, when there is no network", async () => {
    let up = false;
    const old = idToken("old", T0 - 5);
    const t = setup({ id: old, refresh: "rt0", answer: async (_rt, call) => { if (!up) throw new Error("network"); return { idToken: idToken(`r${call}`, T0 + 3600), refreshToken: "rt9" }; } });
    await t.session.restore();
    expect(t.session.getState()).toEqual({ status: "signedIn", token: old });
    expect(t.rtStore.value).toBe("rt0");
    // The first request tries again, and this time it works.
    up = true;
    const token = await t.session.getTokenForRequest();
    expect(token).not.toBe(old);
    expect(t.session.getToken()).toBe(token);
  });

  it("signs out when the refreshed token is itself unusable", async () => {
    const t = setup({ id: null, refresh: "rt0", answer: async () => ({ idToken: idToken("x", T0 - 1), refreshToken: "rt1" }) });
    await t.session.restore();
    expect(t.session.getState().status).toBe("signedOut");
  });
});

describe("getTokenForRequest", () => {
  it("returns the ID token as it is while it has more than a minute left", async () => {
    const t = setup();
    const tok = idToken("a", T0 + 120);
    await t.session.signIn(tok, "rt0");
    expect(await t.session.getTokenForRequest()).toBe(tok);
    expect(t.refresher.calls).toEqual([]);
  });

  it("refreshes first when the token is about to expire, and keeps the rotated refresh token", async () => {
    const t = setup();
    await t.session.signIn(idToken("a", T0 + 120), "rt0");
    t.advance(100); // 20 s left: inside the 60 s margin
    const token = await t.session.getTokenForRequest();
    expect(t.refresher.calls).toEqual(["rt0"]);
    expect(token).toBe(t.session.getToken());
    expect(token).not.toBeNull();
    await settle();
    expect(t.rtStore.value).toBe("rt1");
    expect(t.idStore.value).toBe(token);
  });

  it("shares ONE refresh between concurrent requests (the provider rotates the token)", async () => {
    let release!: (t: Tokens) => void;
    const t = setup({ answer: () => new Promise<Tokens>((r) => (release = r)) });
    await t.session.signIn(idToken("a", T0 + 45), "rt0");
    const calls = [t.session.getTokenForRequest(), t.session.getTokenForRequest(), t.session.getTokenForRequest()];
    await settle();
    expect(t.refresher.calls).toEqual(["rt0"]); // not three
    const fresh = idToken("new", T0 + 3600);
    release({ idToken: fresh, refreshToken: "rt1" });
    expect(await Promise.all(calls)).toEqual([fresh, fresh, fresh]);
  });

  it("signs out and returns no token when the refresh token is refused", async () => {
    const t = setup({ answer: async () => { throw new RefreshRejectedError("invalid_grant"); } });
    await t.session.signIn(idToken("a", T0 + 45), "rt0");
    expect(await t.session.getTokenForRequest()).toBeNull();
    expect(t.session.getState().status).toBe("signedOut");
    expect(t.idStore.value).toBeNull();
    expect(t.rtStore.value).toBeNull();
  });

  it("fails only that call on a transient error, and tries again on the next", async () => {
    let fail = true;
    const t = setup({ answer: async (_rt, call) => { if (fail) throw new Error("network"); return { idToken: idToken(`r${call}`, T0 + 3600), refreshToken: "rt2" }; } });
    const old = idToken("a", T0 + 45);
    await t.session.signIn(old, "rt0");
    await expect(t.session.getTokenForRequest()).rejects.toThrow("network");
    expect(t.session.getState()).toEqual({ status: "signedIn", token: old }); // untouched
    expect(t.rtStore.value).toBe("rt0");
    fail = false;
    expect(await t.session.getTokenForRequest()).not.toBe(old);
  });

  it("discards a refresh that finishes after a sign-out", async () => {
    let release!: (t: Tokens) => void;
    const t = setup({ answer: () => new Promise<Tokens>((r) => (release = r)) });
    await t.session.signIn(idToken("a", T0 + 45), "rt0");
    const pending = t.session.getTokenForRequest();
    await settle();
    await t.session.signOut();
    release({ idToken: idToken("late", T0 + 3600), refreshToken: "rt-late" });
    expect(await pending).toBeNull();
    await settle();
    expect(t.session.getState().status).toBe("signedOut");
    expect(t.idStore.value).toBeNull();
    expect(t.rtStore.value).toBeNull(); // the late token did not come back
  });

  it("discards a refresh that finishes after a newer login", async () => {
    let release!: (t: Tokens) => void;
    const t = setup({ answer: () => new Promise<Tokens>((r) => (release = r)) });
    await t.session.signIn(idToken("first", T0 + 45), "rt-first");
    const pending = t.session.getTokenForRequest();
    await settle();
    const second = idToken("second", T0 + 3600);
    await t.session.signIn(second, "rt-second");
    release({ idToken: idToken("late", T0 + 3600), refreshToken: "rt-late" });
    await pending;
    await settle();
    expect(t.session.getState()).toEqual({ status: "signedIn", token: second });
    expect(t.rtStore.value).toBe("rt-second");
  });

  it("without a refresh token it returns the token as it is, even close to expiry", async () => {
    const t = setup();
    const tok = idToken("a", T0 + 45);
    await t.session.signIn(tok, null);
    expect(await t.session.getTokenForRequest()).toBe(tok);
    expect(t.refresher.calls).toEqual([]);
  });

  it("returns null when signed out", async () => {
    const t = setup();
    await t.session.restore();
    expect(await t.session.getTokenForRequest()).toBeNull();
  });
});

describe("a 401 from the api (expire)", () => {
  it("tries one refresh and stays signed in", async () => {
    const t = setup();
    const tok = idToken("a", T0 + 3600); // not expired as far as the app knows
    await t.session.signIn(tok, "rt0");
    await t.session.expire(tok);
    expect(t.refresher.calls).toEqual(["rt0"]);
    expect(t.session.getState().status).toBe("signedIn");
    expect(t.session.getToken()).not.toBe(tok);
  });

  it("ends the session when the token that failed was made by a refresh a moment ago", async () => {
    const t = setup();
    await t.session.signIn(idToken("a", T0 + 3600), "rt0");
    await t.session.expire(t.session.getToken());
    const refreshed = t.session.getToken();
    t.advance(5);
    await t.session.expire(refreshed); // the api rejects the NEW token too: not an expiry problem
    expect(t.session.getState().status).toBe("signedOut");
    expect(t.refresher.calls).toHaveLength(1); // no second refresh
  });

  it("refreshes again for a later 401", async () => {
    const t = setup();
    await t.session.signIn(idToken("a", T0 + 3600), "rt0");
    await t.session.expire(t.session.getToken());
    t.advance(120);
    await t.session.expire(t.session.getToken());
    expect(t.refresher.calls).toEqual(["rt0", "rt1"]);
    expect(t.session.getState().status).toBe("signedIn");
  });

  it("signs out when the refresh token is refused", async () => {
    const t = setup({ answer: async () => { throw new RefreshRejectedError("invalid_grant"); } });
    const tok = idToken("a", T0 + 3600);
    await t.session.signIn(tok, "rt0");
    await t.session.expire(tok);
    expect(t.session.getState().status).toBe("signedOut");
  });

  it("stays signed in on a transient failure", async () => {
    const t = setup({ answer: async () => { throw new Error("network"); } });
    const tok = idToken("a", T0 + 3600);
    await t.session.signIn(tok, "rt0");
    await t.session.expire(tok);
    expect(t.session.getState()).toEqual({ status: "signedIn", token: tok });
  });

  it("signs out as before when there is no refresh token", async () => {
    const t = setup();
    const tok = idToken("a", T0 + 3600);
    await t.session.signIn(tok, null);
    await t.session.expire(tok);
    expect(t.session.getState().status).toBe("signedOut");
    expect(t.refresher.calls).toEqual([]);
  });

  it("still ignores the late 401 of a request made under an older token", async () => {
    const t = setup();
    await t.session.signIn(idToken("current", T0 + 3600), "rt0");
    await t.session.expire(idToken("older", T0 + 3600));
    expect(t.refresher.calls).toEqual([]);
    expect(t.session.getState().status).toBe("signedIn");
  });
});

describe("signIn and signOut with a refresh token", () => {
  it("saves both tokens", async () => {
    const t = setup();
    const tok = idToken("a", T0 + 3600);
    await t.session.signIn(tok, "rt0");
    expect(t.idStore.value).toBe(tok);
    expect(t.rtStore.value).toBe("rt0");
  });

  it("clears both on sign-out, memory first, even when one clear fails", async () => {
    const t = setup();
    await t.session.signIn(idToken("a", T0 + 3600), "rt0");
    t.rtStore.failClear = true;
    await t.session.signOut();
    expect(t.session.getState().status).toBe("signedOut");
    expect(t.idStore.value).toBeNull();
    expect(t.rtStore.clears).toBe(1);
    expect(await t.session.getTokenForRequest()).toBeNull();
  });
});
