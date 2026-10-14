CREATE TYPE pin_glide.identity_sender_registration_state
AS ENUM ('active', 'revoked');

COMMENT ON TYPE pin_glide.identity_sender_registration_state
IS 'Lifecycle of a Sender registration: active or security-revoked.';
