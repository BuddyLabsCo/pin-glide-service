CREATE TYPE pin_glide.pairing_state AS ENUM ('pending', 'active', 'removed');

COMMENT ON TYPE pin_glide.pairing_state
IS 'Association lifecycle: pending, active, or removed.';
