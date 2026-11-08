/**
 * @jest-environment node
 */
import { balanceSentence, transferSentence } from "./sentences";

describe("balanceSentence", () => {
  it("says who is owed, who owes and who is settled", () => {
    expect(balanceSentence("Alice", false, 500, "CAD")).toBe("Alice is owed $5.00 CAD");
    expect(balanceSentence("Alice", false, -500, "USD")).toBe("Alice owes $5.00 USD");
    expect(balanceSentence("Alice", false, 0, "CAD")).toBe("Alice is settled up");
  });

  it("says you for the signed-in user", () => {
    expect(balanceSentence("Alice", true, 1, "CAD")).toBe("You are owed $0.01 CAD");
    expect(balanceSentence("Alice", true, -123456, "CAD")).toBe("You owe $1,234.56 CAD");
    expect(balanceSentence("Alice", true, 0, "CAD")).toBe("You are settled up");
  });
});

describe("transferSentence", () => {
  const me = { name: "Alice", isMe: true };
  const bob = { name: "Bob", isMe: false };
  const carol = { name: "Carol", isMe: false };

  it("reads from the point of view of the signed-in user", () => {
    expect(transferSentence(me, bob, 1250, "CAD")).toBe("You pay Bob $12.50 CAD");
    expect(transferSentence(bob, me, 1250, "CAD")).toBe("Bob pays you $12.50 CAD");
    expect(transferSentence(bob, carol, 1250, "CAD")).toBe("Bob pays Carol $12.50 CAD");
  });
});
