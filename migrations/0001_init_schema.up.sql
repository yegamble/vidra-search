-- 0001: bootstrap the search service's storage.
--
-- Extensions are guarded (IF NOT EXISTS) because vidra-search may share a
-- database with vidra-core, where uuid-ossp/pg_trgm are already installed. We
-- only require pg_trgm (fuzzy prefix + typo fallback for suggestions/search);
-- uuid-ossp is created best-effort so server-side uuid_generate_v4() is
-- available for any future default without depending on core's migrations.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- All vidra-search tables live in a dedicated `search` schema so the service can
-- coexist with vidra-core in one database without name collisions. The runtime
-- pool sets search_path=search,public; every table is additionally
-- schema-qualified in SQL so resolution never depends on the path.
CREATE SCHEMA IF NOT EXISTS search;
