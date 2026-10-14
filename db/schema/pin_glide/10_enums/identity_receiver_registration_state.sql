CREATE TYPE pin_glide.identity_receiver_registration_state
AS ENUM ('pending', 'active', 'revoked');

COMMENT ON TYPE pin_glide.identity_receiver_registration_state
IS 'Receiver registration lifecycle: pending, active, or revoked.';
