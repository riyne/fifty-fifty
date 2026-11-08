import { session } from "../auth";
import { API_URL } from "../config";
import { createApi } from "./client";

/** The app's api client: sends the session's token, and ends the session on a 401. */
export const api = createApi({
  baseUrl: API_URL,
  // Refreshes the ID token first when it is about to expire (one refresh shared by concurrent requests).
  getToken: session.getTokenForRequest,
  onUnauthorized: (sentToken) => {
    void session.expire(sentToken);
  },
});
