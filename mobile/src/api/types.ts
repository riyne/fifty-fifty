// Names for the response types the screens use, taken from the generated schema so they can
// not drift from api/openapi.yaml.
import type { components, paths } from "./schema";

export type Me = components["schemas"]["Me"];
export type Group = components["schemas"]["Group"];
export type Balances = components["schemas"]["Balances"];
export type CreatedJoinCode = components["schemas"]["CreatedJoinCode"];
export type GroupDetail = paths["/v1/groups/{id}"]["get"]["responses"]["200"]["content"]["application/json"];
export type Expense = components["schemas"]["Expense"];
export type ExpenseSummary = components["schemas"]["ExpenseSummary"];
export type ExpenseItem = components["schemas"]["ExpenseItem"];
export type ExpenseStatus = Expense["status"];
export type Settlement = components["schemas"]["Settlement"];
export type SettlementStatus = Settlement["status"];
export type { Currency } from "./money";
