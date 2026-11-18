DROP TABLE IF EXISTS tuple_backoff;

ALTER TABLE coins DROP COLUMN IF EXISTS confirmed_mtp;

ALTER TABLE delegations ADD COLUMN address text NOT NULL DEFAULT '';

UPDATE delegations AS d
SET address = c.address
FROM claims AS c
WHERE c.delegation_id = d.id AND c.input = 0;

CREATE INDEX delegations_address_idx ON delegations (address);

DROP INDEX IF EXISTS claims_address_idx;

ALTER TABLE claims DROP COLUMN IF EXISTS address;
ALTER TABLE claims DROP COLUMN IF EXISTS sequence;
ALTER TABLE claims DROP COLUMN IF EXISTS tx_locktime;
