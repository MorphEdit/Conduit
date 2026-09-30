-- Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
-- Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
-- Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

-- Sample tables for the example. Every synced table needs a primary key.
CREATE TABLE customers (
    id         SERIAL PRIMARY KEY,
    code       TEXT UNIQUE,
    name       TEXT NOT NULL,
    email      TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id          SERIAL PRIMARY KEY,
    customer_id INT REFERENCES customers(id),
    total       NUMERIC(14,2) NOT NULL DEFAULT 0,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO customers (code, name, email) VALUES
  ('C-001', 'First customer', 'first@example.com'),
  ('C-002', 'ลูกค้าคนที่สอง', NULL);
