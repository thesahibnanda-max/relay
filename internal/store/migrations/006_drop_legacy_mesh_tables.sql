-- Numbers 3-5 were the multi-machine "mesh" feature's migrations
-- (mesh_sessions/mesh_peers, mesh_agents, and a messages rebuild), fully
-- reverted in commit 10f2b8a. Any real database that ran them is
-- permanently stuck at PRAGMA user_version 3, 4 or 5, with these leftover
-- tables still on disk - reverting the code never undoes their schema
-- effect on an already-migrated database. mesh_sessions.session_id
-- references sessions(id) with no ON DELETE clause, so a leftover row there
-- blocks deleting the session it names (this is what made relay gc fail
-- with a FOREIGN KEY constraint error on such a database).
--
-- This migration cleans that up and is a safe no-op on any database that
-- never ran the mesh migrations. Never reuse 3, 4 or 5 for a new migration -
-- see store.go's migrate()/SchemaVersion() comments for why.
DROP TABLE IF EXISTS mesh_peers;
DROP TABLE IF EXISTS mesh_agents;
DROP TABLE IF EXISTS mesh_sessions;
