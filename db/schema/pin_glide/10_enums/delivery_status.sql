CREATE TYPE pin_glide.delivery_status AS ENUM (
    'processing',
    'received',
    'offline',
    'send_failed',
    'ack_timeout',
    'interrupted'
);

COMMENT ON TYPE pin_glide.delivery_status
IS 'Delivery lifecycle: processing until one terminal outcome is recorded.';
