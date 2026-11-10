-- Fixture schema for UpdateBatchCAS generation and functional tests.
-- Composite primary key (text + integer) exercises type-fidelious identity
-- checks; nullable columns cover null data paths; the narrow int16 version
-- column exercises small-range overflow rejection.

CREATE TABLE cas_items (
    tenant_id TEXT    NOT NULL,
    item_id   INTEGER NOT NULL,
    name      TEXT    NOT NULL,
    note      TEXT,
    weight    REAL,
    raw       BLOB,
    version   INTEGER NOT NULL,
    PRIMARY KEY (tenant_id, item_id)
);

CREATE TABLE cas_counters (
    id      INTEGER PRIMARY KEY,
    label   TEXT    NOT NULL,
    value   INTEGER NOT NULL DEFAULT 0,
    version INTEGER NOT NULL
);

CREATE TABLE cas_ticks (
    id      INTEGER PRIMARY KEY,
    step    SMALLINT NOT NULL,
    version SMALLINT NOT NULL
);
