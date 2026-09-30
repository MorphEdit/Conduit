-- Same schema on every node (Conduit replicates rows, not DDL).
-- Column types are deliberately varied to exercise text-format apply.
CREATE TABLE customers (
    id         SERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    email      TEXT,
    vip        BOOLEAN NOT NULL DEFAULT false,
    tags       TEXT[],
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE quotations (
    id          SERIAL PRIMARY KEY,
    customer_id INT REFERENCES customers(id),
    doc_no      VARCHAR(30) NOT NULL,
    total       NUMERIC(14,2) NOT NULL DEFAULT 0,
    meta        JSONB,
    issued_on   DATE,
    attachment  BYTEA,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Composite primary key.
CREATE TABLE quotation_items (
    quotation_id INT NOT NULL REFERENCES quotations(id) ON DELETE CASCADE,
    line_no      INT NOT NULL,
    description  TEXT NOT NULL,
    qty          NUMERIC(10,2) NOT NULL,
    unit_price   NUMERIC(14,2) NOT NULL,
    PRIMARY KEY (quotation_id, line_no)
);
