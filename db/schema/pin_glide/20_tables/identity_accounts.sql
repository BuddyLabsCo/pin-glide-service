CREATE TABLE IF NOT EXISTS pin_glide.identity_accounts (
    id text NOT NULL,
    wechat_app_id text NOT NULL,
    wechat_open_id text NOT NULL,
    wechat_union_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_identity_accounts PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS
uidx_identity_accounts_wechat_app_id_wechat_open_id
ON pin_glide.identity_accounts (wechat_app_id, wechat_open_id);

COMMENT ON TABLE pin_glide.identity_accounts
IS 'Configuration Data: one Account per verified WeChat identity, no cleanup.';
COMMENT ON COLUMN pin_glide.identity_accounts.id
IS 'Internal opaque Account identifier issued by the service.';
COMMENT ON COLUMN pin_glide.identity_accounts.wechat_app_id
IS 'Configured WeChat AppID that verified this OpenID.';
COMMENT ON COLUMN pin_glide.identity_accounts.wechat_open_id
IS 'WeChat OpenID verified for the configured AppID.';
COMMENT ON COLUMN pin_glide.identity_accounts.wechat_union_id
IS 'Optional WeChat UnionID, recorded only; it never merges Accounts.';
COMMENT ON COLUMN pin_glide.identity_accounts.created_at
IS 'When the Account was first created.';
