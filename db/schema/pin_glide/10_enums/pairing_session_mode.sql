CREATE TYPE pin_glide.pairing_session_mode AS ENUM (
    'new_receiver',
    'add_sender',
    'recover_receiver'
);

COMMENT ON TYPE pin_glide.pairing_session_mode
IS 'Fixed purpose of one Pairing Session; not switchable later.';
