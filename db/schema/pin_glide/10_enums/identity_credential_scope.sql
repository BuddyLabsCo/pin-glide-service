CREATE TYPE pin_glide.identity_credential_scope AS ENUM (
    'user_session',
    'uploader',
    'refresh'
);

COMMENT ON TYPE pin_glide.identity_credential_scope
IS 'Authority of one Sender credential: management, upload, or renewal.';
