CREATE TABLE IF NOT EXISTS pin_glide.identity_accounts (
    id text NOT NULL,
    provider text NOT NULL,
    app_id text NOT NULL,
    open_id text NOT NULL,
    union_id text,
    nickname text,
    avatar_url text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_identity_accounts PRIMARY KEY (id),
    CONSTRAINT chk_identity_accounts_provider CHECK (
        provider IN ('wechat', 'qq')
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS
uidx_identity_accounts_provider_app_id_open_id
ON pin_glide.identity_accounts (provider, app_id, open_id);

COMMENT ON TABLE pin_glide.identity_accounts
IS 'Configuration Data: one Account per provider identity, no cleanup.';
COMMENT ON COLUMN pin_glide.identity_accounts.id
IS 'Internal opaque Account identifier issued by the service.';
COMMENT ON COLUMN pin_glide.identity_accounts.provider
IS 'Identity provider namespace: wechat or qq.';
COMMENT ON COLUMN pin_glide.identity_accounts.app_id
IS 'Configured provider application that verified this OpenID.';
COMMENT ON COLUMN pin_glide.identity_accounts.open_id
IS 'Provider-verified OpenID within its provider application namespace.';
COMMENT ON COLUMN pin_glide.identity_accounts.union_id
IS 'Optional provider UnionID, recorded only; it never merges Accounts.';
COMMENT ON COLUMN pin_glide.identity_accounts.nickname
IS 'Cached display nickname from successful QQ login; NULL if unavailable.';
COMMENT ON COLUMN pin_glide.identity_accounts.avatar_url
IS 'Cached QQ avatar URL from successful login; NULL if unavailable.';
COMMENT ON COLUMN pin_glide.identity_accounts.created_at
IS 'When the Account was first created.';
