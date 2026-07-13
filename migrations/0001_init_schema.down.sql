-- Drop the search schema and everything in it. Extensions are left in place:
-- they may be shared with vidra-core, so dropping them here could break core.
DROP SCHEMA IF EXISTS search CASCADE;
