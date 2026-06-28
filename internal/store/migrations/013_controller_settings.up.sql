CREATE TABLE controller_settings (
    id                INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    controller_key_hex TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
