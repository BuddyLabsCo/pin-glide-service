CREATE TABLE IF NOT EXISTS pin_glide.service_diagnostics (
    id bigserial,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    request_id text NOT NULL,
    kind text NOT NULL,
    subject text,
    method text,
    code smallint,
    duration_ms bigint,
    CONSTRAINT pk_service_diagnostics PRIMARY KEY (id),
    CONSTRAINT chk_service_diagnostics_kind CHECK (kind IN ('http', 'wss')),
    -- An HTTP status is 100-599 and a WebSocket Close status is 1000-4999, so
    -- one column records either meaning the surface that produced it defines.
    CONSTRAINT chk_service_diagnostics_code_range CHECK (
        code IS NULL OR code BETWEEN 100 AND 4999
    ),
    CONSTRAINT chk_service_diagnostics_duration_ms_not_negative CHECK (
        duration_ms IS NULL OR duration_ms >= 0
    )
);

-- The retention clock of this table, so scheduled cleanup can delete by age.
CREATE INDEX IF NOT EXISTS idx_service_diagnostics_occurred_at
ON pin_glide.service_diagnostics (occurred_at);

-- One operation correlates every record it produced, so the identifier the
-- HTTP response and the service log state is readable here too.
CREATE INDEX IF NOT EXISTS idx_service_diagnostics_request_id
ON pin_glide.service_diagnostics (request_id);

COMMENT ON TABLE pin_glide.service_diagnostics
IS 'Log Data: safe service outcome records on a thirty-day clock.';
COMMENT ON COLUMN pin_glide.service_diagnostics.id
IS 'Surrogate key of one diagnostic record.';
COMMENT ON COLUMN pin_glide.service_diagnostics.occurred_at
IS 'When the recorded operation ended; the retention clock of this table.';
COMMENT ON COLUMN pin_glide.service_diagnostics.request_id
IS 'Operation the service generated; it correlates response, log and record.';
COMMENT ON COLUMN pin_glide.service_diagnostics.kind
IS 'Surface that produced the outcome: HTTP request or WSS connection.';
COMMENT ON COLUMN pin_glide.service_diagnostics.subject
IS 'Safe Receiver or Pairing Session identifier; NULL when there is none.';
COMMENT ON COLUMN pin_glide.service_diagnostics.method
IS 'HTTP method of the recorded request; NULL for a connection record.';
COMMENT ON COLUMN pin_glide.service_diagnostics.code
IS 'HTTP status or WSS Close code; NULL while the operation has none.';
COMMENT ON COLUMN pin_glide.service_diagnostics.duration_ms
IS 'How long the recorded request took; NULL for a connection record.';
