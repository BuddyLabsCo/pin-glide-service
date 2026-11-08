ALTER TABLE pin_glide.identity_sender_registrations
ADD COLUMN IF NOT EXISTS device_name text;

-- Operators must supply names before Schema Sync can enable the invariant.
-- Never guess hardware metadata or discard an unnamed registration.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pin_glide.identity_sender_registrations
        WHERE device_name IS NULL
           OR char_length(device_name) NOT BETWEEN 1 AND 64
           OR device_name <> trim(
               chr(9) || chr(10) || chr(11) || chr(12)
               || chr(13) || chr(32) || chr(133) || chr(160)
               || chr(5760) || chr(8192) || chr(8193) || chr(8194)
               || chr(8195) || chr(8196) || chr(8197) || chr(8198)
               || chr(8199) || chr(8200) || chr(8201) || chr(8202)
               || chr(8232) || chr(8233) || chr(8239) || chr(8287)
               || chr(12288) FROM device_name
           )
    ) THEN
        RAISE EXCEPTION
            'Populate valid Sender device names before upgrading; see %',
            'docs/upgrades/issue-109-sender-names.md';
    END IF;
END
$$;
