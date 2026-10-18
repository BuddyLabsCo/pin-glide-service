DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'pin_glide'
          AND table_name = 'identity_accounts'
          AND column_name = 'wechat_app_id'
    ) AND NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'pin_glide'
          AND table_name = 'identity_accounts'
          AND column_name = 'app_id'
    ) THEN
        ALTER TABLE pin_glide.identity_accounts
        RENAME COLUMN wechat_app_id TO app_id;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'pin_glide'
          AND table_name = 'identity_accounts'
          AND column_name = 'wechat_open_id'
    ) AND NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'pin_glide'
          AND table_name = 'identity_accounts'
          AND column_name = 'open_id'
    ) THEN
        ALTER TABLE pin_glide.identity_accounts
        RENAME COLUMN wechat_open_id TO open_id;
    END IF;

    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'pin_glide'
          AND table_name = 'identity_accounts'
          AND column_name = 'wechat_union_id'
    ) AND NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'pin_glide'
          AND table_name = 'identity_accounts'
          AND column_name = 'union_id'
    ) THEN
        ALTER TABLE pin_glide.identity_accounts
        RENAME COLUMN wechat_union_id TO union_id;
    END IF;
END
$$;

ALTER TABLE pin_glide.identity_accounts
ADD COLUMN IF NOT EXISTS provider text;

UPDATE pin_glide.identity_accounts
SET provider = 'wechat'
WHERE provider IS NULL;

ALTER TABLE pin_glide.identity_accounts
ALTER COLUMN provider SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'pin_glide.identity_accounts'::regclass
          AND conname = 'chk_identity_accounts_provider'
    ) THEN
        ALTER TABLE pin_glide.identity_accounts
        ADD CONSTRAINT chk_identity_accounts_provider CHECK (
            provider IN ('wechat', 'qq')
        );
    END IF;
END
$$;

DROP INDEX IF EXISTS
pin_glide.uidx_identity_accounts_wechat_app_id_wechat_open_id;

CREATE UNIQUE INDEX IF NOT EXISTS
uidx_identity_accounts_provider_app_id_open_id
ON pin_glide.identity_accounts (provider, app_id, open_id);
