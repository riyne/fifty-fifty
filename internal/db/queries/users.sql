-- name: GetUserByIdentity :one
SELECT * FROM users WHERE issuer = $1 AND subject = $2;

-- EnsureUser is race-safe first-login provisioning. The no-op DO UPDATE makes
-- RETURNING yield the existing row when a concurrent request inserted first,
-- which DO NOTHING would not. Callers try GetUserByIdentity first so the hot
-- path does not write.
-- name: EnsureUser :one
INSERT INTO users (issuer, subject, email, display_name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (issuer, subject) DO UPDATE SET issuer = EXCLUDED.issuer
RETURNING *;
