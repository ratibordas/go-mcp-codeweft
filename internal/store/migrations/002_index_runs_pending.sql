ALTER TABLE index_runs ADD COLUMN IF NOT EXISTS pending Array(String) DEFAULT [];
