ALTER TABLE claims
    ADD COLUMN address text NOT NULL DEFAULT '',
    ADD COLUMN sequence bigint NOT NULL DEFAULT 4294967295,
    ADD COLUMN tx_locktime bigint NOT NULL DEFAULT 0;

UPDATE claims AS c
SET address = d.address,
    tx_locktime = CASE WHEN c.locktime <> 0 THEN c.locktime ELSE c.tx_locktime END
FROM delegations AS d
WHERE d.id = c.delegation_id AND c.input = 0;

UPDATE claims SET tx_locktime = locktime WHERE tx_locktime = 0 AND locktime <> 0;

CREATE INDEX claims_address_idx ON claims (address) WHERE address <> '';

UPDATE delegations
SET inputs = jsonb_set(inputs, '{0,Address}', to_jsonb(address), true)
WHERE address <> ''
  AND jsonb_typeof(inputs) = 'array'
  AND jsonb_array_length(inputs) > 0
  AND coalesce(inputs -> 0 ->> 'Address', '') = '';

DROP INDEX IF EXISTS delegations_address_idx;
ALTER TABLE delegations DROP COLUMN address;

ALTER TABLE coins ADD COLUMN confirmed_mtp timestamptz NULL;

CREATE TABLE tuple_backoff (
    delegation_id bigint NOT NULL REFERENCES delegations (id) ON DELETE CASCADE,
    coins text[] NOT NULL,
    retry_at timestamptz NOT NULL,
    PRIMARY KEY (delegation_id, coins)
);

CREATE INDEX tuple_backoff_retry_at_idx ON tuple_backoff (retry_at);
