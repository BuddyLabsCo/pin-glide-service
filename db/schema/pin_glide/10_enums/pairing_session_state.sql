CREATE TYPE pin_glide.pairing_session_state AS ENUM (
    'awaiting_authorization',
    'awaiting_confirmation',
    'credential_ready',
    'credential_issued',
    'completed',
    'canceled',
    'expired',
    'superseded'
);

COMMENT ON TYPE pin_glide.pairing_session_state
IS 'Adopted Pairing Session lifecycle; see the state comments.';
