CREATE INDEX delegations_client_active_idx ON delegations (created_at) WHERE stream_id IS NULL AND status = 'active';
CREATE INDEX delegations_client_expired_idx ON delegations (updated_at) WHERE stream_id IS NULL AND status = 'expired';
