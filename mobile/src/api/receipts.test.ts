import { receiptForm } from "./receipts";

// expo-file-system's File needs the native module, and Jest's FormData is Node's. Stand-ins show
// what is appended; that the real File is read and sent is only seen when the app runs.
jest.mock("expo-file-system", () => ({
  File: class {
    uri: string;
    constructor(uri: string) {
      this.uri = uri;
    }
  },
}));

class RecordingFormData {
  readonly parts: [string, unknown][] = [];
  append(name: string, value: unknown) {
    this.parts.push([name, value]);
  }
}

const original = globalThis.FormData;
beforeEach(() => {
  globalThis.FormData = RecordingFormData as unknown as typeof FormData;
});
afterEach(() => {
  globalThis.FormData = original;
});

const parts = (form: FormData) => (form as unknown as RecordingFormData).parts;

describe("receiptForm", () => {
  it("sends the file as a File part made from its local uri", () => {
    const [[name, file], ...rest] = parts(receiptForm({ uri: "file:///cache/a.jpg" }, ""));
    expect(name).toBe("file");
    expect(file).toMatchObject({ uri: "file:///cache/a.jpg" });
    expect(rest).toEqual([]); // no description part: the api's default, "Receipt", applies
  });

  it("adds the description trimmed, and leaves it out when blank", () => {
    expect(parts(receiptForm({ uri: "file:///a.jpg" }, "  Dinner at Nando's "))[1]).toEqual(["description", "Dinner at Nando's"]);
    expect(parts(receiptForm({ uri: "file:///a.jpg" }, "   "))).toHaveLength(1);
  });
});
