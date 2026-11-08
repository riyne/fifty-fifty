/**
 * @jest-environment node
 */
import { createSession, type SessionState, type TokenStore } from "./session";
import { fakeJwt } from "./fakeJwt";

const NOW = 1_000_000;
const good = (tag = "a") => fakeJwt({ sub: tag, exp: NOW + 3600 });
const expired = () => fakeJwt({ sub: "old", exp: NOW - 10 });

/** A TokenStore in a variable, with switches to make each call fail. */
function fakeStore(initial: string | null = null) {
  const calls: string[] = [];
  const store = {
    value: initial,
    failLoad: false,
    failSave: false,
    failClear: false,
    async load() {
      calls.push("load");
      if (store.failLoad) throw new Error("keychain");
      return store.value;
    },
    async save(token: string) {
      calls.push("save");
      if (store.failSave) throw new Error("keychain");
      store.value = token;
    },
    async clear() {
      calls.push("clear");
      if (store.failClear) throw new Error("keychain");
      store.value = null;
    },
  };
  return { store: store satisfies TokenStore, calls };
}

const make = (store: TokenStore) => createSession(store, () => NOW);

describe("restore", () => {
  it("starts as loading, with no token", () => {
    const s = make(fakeStore().store);
    expect(s.getState()).toEqual({ status: "loading" });
    expect(s.getToken()).toBeNull();
  });

  it("signs in with a stored token that is still usable", async () => {
    const token = good();
    const s = make(fakeStore(token).store);
    await s.restore();
    expect(s.getState()).toEqual({ status: "signedIn", token });
    expect(s.getToken()).toBe(token);
  });

  it("signs out when nothing is stored", async () => {
    const f = fakeStore(null);
    const s = make(f.store);
    await s.restore();
    expect(s.getState()).toEqual({ status: "signedOut" });
    expect(f.calls).toEqual(["load"]); // nothing to clear
  });

  it("drops an expired stored token from the store and signs out", async () => {
    const f = fakeStore(expired());
    const s = make(f.store);
    await s.restore();
    expect(s.getState()).toEqual({ status: "signedOut" });
    expect(s.getToken()).toBeNull();
    expect(f.store.value).toBeNull();
  });

  it("drops a stored value that is not a token", async () => {
    const f = fakeStore("garbage");
    const s = make(f.store);
    await s.restore();
    expect(s.getState().status).toBe("signedOut");
    expect(f.store.value).toBeNull();
  });

  it("signs out when the keychain cannot be read", async () => {
    const f = fakeStore(good());
    f.store.failLoad = true;
    const s = make(f.store);
    await s.restore();
    expect(s.getState()).toEqual({ status: "signedOut" });
  });

  it("does not undo a login that happened while it was reading", async () => {
    let release!: (value: string | null) => void;
    const slow: TokenStore = {
      load: () => new Promise((resolve) => (release = resolve)),
      save: async () => {},
      clear: async () => {},
    };
    const s = make(slow);
    const restoring = s.restore();
    const token = good("fresh");
    await s.signIn(token);
    release(expired());
    await restoring;
    expect(s.getState()).toEqual({ status: "signedIn", token });
  });
});

describe("signIn", () => {
  it("signs in and saves the token", async () => {
    const f = fakeStore();
    const s = make(f.store);
    const token = good();
    await s.signIn(token);
    expect(s.getState()).toEqual({ status: "signedIn", token });
    expect(f.store.value).toBe(token);
  });

  it("refuses an expired or unreadable token and changes nothing", async () => {
    const f = fakeStore();
    const s = make(f.store);
    await s.restore();
    await expect(s.signIn(expired())).rejects.toThrow("unusable token");
    await expect(s.signIn("garbage")).rejects.toThrow("unusable token");
    expect(s.getState()).toEqual({ status: "signedOut" });
    expect(f.store.value).toBeNull();
  });

  it("stays signed in for this run when the keychain cannot save", async () => {
    const f = fakeStore();
    f.store.failSave = true;
    const s = make(f.store);
    const token = good();
    await s.signIn(token);
    expect(s.getToken()).toBe(token);
    expect(f.store.value).toBeNull();
  });
});

describe("signOut", () => {
  it("drops the token from memory before the keychain call finishes", async () => {
    let finishClear!: () => void;
    const seen: SessionState[] = [];
    const store: TokenStore = {
      load: async () => null,
      save: async () => {},
      clear: () => new Promise<void>((resolve) => (finishClear = resolve)),
    };
    const s = make(store);
    await s.signIn(good());
    const pending = s.signOut();
    seen.push(s.getState());
    expect(s.getToken()).toBeNull();
    finishClear();
    await pending;
    expect(seen).toEqual([{ status: "signedOut" }]);
  });

  it("clears the store", async () => {
    const f = fakeStore(good());
    const s = make(f.store);
    await s.restore();
    await s.signOut();
    expect(f.store.value).toBeNull();
  });

  it("signs out even when the keychain cannot clear", async () => {
    const f = fakeStore(good());
    const s = make(f.store);
    await s.restore();
    f.store.failClear = true;
    await expect(s.signOut()).resolves.toBeUndefined();
    expect(s.getState()).toEqual({ status: "signedOut" });
  });
});

describe("expire (a 401 from the api)", () => {
  it("signs out when the token that failed is the one in use", async () => {
    const f = fakeStore();
    const s = make(f.store);
    const token = good();
    await s.signIn(token);
    await s.expire(token);
    expect(s.getState()).toEqual({ status: "signedOut" });
    expect(f.store.value).toBeNull();
  });

  it("ignores the late 401 of a request made under an older login", async () => {
    const s = make(fakeStore().store);
    const first = good("first");
    const second = good("second");
    await s.signIn(first);
    await s.signIn(second);
    await s.expire(first);
    expect(s.getState()).toEqual({ status: "signedIn", token: second });
  });

  it("ignores a 401 for a request that carried no token, and one when signed out", async () => {
    const s = make(fakeStore().store);
    const token = good();
    await s.signIn(token);
    await s.expire(null);
    expect(s.getToken()).toBe(token);
    await s.signOut();
    await expect(s.expire(token)).resolves.toBeUndefined();
    expect(s.getState()).toEqual({ status: "signedOut" });
  });

  it("works when passed around as a callback", async () => {
    const s = make(fakeStore().store);
    const token = good();
    await s.signIn(token);
    const { expire } = s;
    await expire(token);
    expect(s.getToken()).toBeNull();
  });
});

describe("subscribe", () => {
  it("tells listeners about each change, and stops after unsubscribe", async () => {
    const s = make(fakeStore().store);
    const seen: string[] = [];
    const off = s.subscribe(() => seen.push(s.getState().status));
    await s.restore();
    await s.signIn(good());
    off();
    await s.signOut();
    expect(seen).toEqual(["signedOut", "signedIn"]);
  });
});
