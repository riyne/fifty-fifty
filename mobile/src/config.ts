// EXPO_PUBLIC_* variables are inlined into the bundle at build time (nothing secret goes here).
// The iOS Simulator shares the host's network, so the api started with `make run-api` is on
// localhost. A physical device would need the host's address, which is not supported.
export const API_URL = process.env.EXPO_PUBLIC_API_URL ?? "http://localhost:8080";
