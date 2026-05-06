-- Create all databases required by f33d3r services.
-- Runs automatically on first postgres container start via /docker-entrypoint-initdb.d/
SELECT 'CREATE DATABASE f33d3r_registry' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_registry')\gexec
SELECT 'CREATE DATABASE f33d3r_feed'     WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_feed')\gexec
SELECT 'CREATE DATABASE f33d3r_wallet'   WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_wallet')\gexec
SELECT 'CREATE DATABASE f33d3r_msg'      WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_msg')\gexec
SELECT 'CREATE DATABASE f33d3r_security' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_security')\gexec
SELECT 'CREATE DATABASE f33d3r_safety'   WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_safety')\gexec
SELECT 'CREATE DATABASE f33d3r_handles'  WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_handles')\gexec
SELECT 'CREATE DATABASE f33d3r_verity'   WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_verity')\gexec
SELECT 'CREATE DATABASE f33d3r_ledger'    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_ledger')\gexec
SELECT 'CREATE DATABASE f33d3r_commerce' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'f33d3r_commerce')\gexec
