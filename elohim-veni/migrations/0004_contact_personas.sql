-- 0004_contact_personas.sql
--
-- A PIAL is a person's root identity, and one person can hold several accounts
-- (personas). The contact plane was keyed by PIAL alone, so every action on it
-- was attributed to the person and never to the persona that took it:
--
--   * a contact request named only requester_pial, and the brain that renders
--     it picked the PIAL's primary account — so a request sent as persona B
--     showed the recipient persona A;
--   * a grant, a capability, a Number and its admission ledger belonged to the
--     PIAL, so a grant given to A also let B straight through, a Number minted
--     as B reached A, and two personas admitted through one Number counted as
--     one;
--   * messaging keys were one row per PIAL, so every persona's key bundle was
--     the same bundle — the plainest link there could be.
--
-- Each of those lets an observer link two of one person's accounts. From here
-- every row records the account that acted (or was acted on). The PIAL columns
-- stay where the identity still owns the row's lifecycle (its cascade from
-- pial_states) and where server-side gating reads the person.
--
-- ── Backfill ────────────────────────────────────────────────────────────────
-- None of these rows recorded the acting account. Each is attributed to its
-- PIAL's primary account (accounts.is_primary; the earliest account when a PIAL
-- has no primary). A row whose PIAL has no account at all names nobody who can
-- act on it and is deleted.

CREATE TEMP TABLE contact_primary_account ON COMMIT DROP AS
SELECT DISTINCT ON (pial_id) pial_id, id AS account_id
  FROM accounts
 ORDER BY pial_id, is_primary DESC, created_at, id;

-- ── contact_policies: one per account ───────────────────────────────────────
-- Each persona has its own @handle, so who may reach it by that handle is its
-- own choice. Every account on a PIAL inherits the PIAL's current row, so no
-- behaviour changes on the day this lands.
CREATE TABLE contact_account_policies (
    account_id            UUID        PRIMARY KEY REFERENCES accounts(id),
    handle_policy         TEXT        NOT NULL DEFAULT 'open'
        CHECK (handle_policy IN ('open','followers','mutuals','number_only','capability_only','closed')),
    default_number_policy TEXT        NOT NULL DEFAULT 'number_only'
        CHECK (default_number_policy IN ('open','followers','mutuals','number_only','capability_only','closed')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO contact_account_policies (account_id, handle_policy, default_number_policy, created_at, updated_at)
SELECT a.id, cp.handle_policy, cp.default_number_policy, cp.created_at, cp.updated_at
  FROM contact_policies cp
  JOIN accounts a ON a.pial_id = cp.pial_id;
DROP TABLE contact_policies;

-- ── number_policies: the persona that holds the Number ──────────────────────
ALTER TABLE number_policies ADD COLUMN account_id UUID REFERENCES accounts(id);
UPDATE number_policies np SET account_id = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = np.pial_id;
DELETE FROM number_policies WHERE account_id IS NULL;
ALTER TABLE number_policies ALTER COLUMN account_id SET NOT NULL;
CREATE INDEX idx_number_policies_account ON number_policies(account_id);

-- ── number_admissions: the persona admitted ─────────────────────────────────
-- An admission is one account let through. Two personas of one person are two
-- admissions, as they are two people to the owner.
ALTER TABLE number_admissions ADD COLUMN initiator_account UUID REFERENCES accounts(id);
UPDATE number_admissions na SET initiator_account = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = na.initiator_pial;
DELETE FROM number_admissions WHERE initiator_account IS NULL;
ALTER TABLE number_admissions DROP CONSTRAINT number_admissions_pkey;
DROP INDEX IF EXISTS idx_number_admissions_initiator;
ALTER TABLE number_admissions DROP COLUMN initiator_pial;
ALTER TABLE number_admissions ALTER COLUMN initiator_account SET NOT NULL;
ALTER TABLE number_admissions ADD PRIMARY KEY (number_node_id, initiator_account);
CREATE INDEX idx_number_admissions_initiator_account ON number_admissions(initiator_account);

-- ── contact_capabilities: the persona that minted the link ──────────────────
ALTER TABLE contact_capabilities ADD COLUMN account_id UUID REFERENCES accounts(id);
UPDATE contact_capabilities c SET account_id = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = c.pial_id;
DELETE FROM contact_capabilities WHERE account_id IS NULL;
ALTER TABLE contact_capabilities ALTER COLUMN account_id SET NOT NULL;
CREATE INDEX idx_contact_caps_account ON contact_capabilities(account_id);

-- ── contact_grants: account to account ──────────────────────────────────────
-- A standing allow given to one persona is given to that persona alone.
ALTER TABLE contact_grants ADD COLUMN owner_account UUID REFERENCES accounts(id);
ALTER TABLE contact_grants ADD COLUMN peer_account  UUID REFERENCES accounts(id);
UPDATE contact_grants g SET owner_account = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = g.owner_pial;
UPDATE contact_grants g SET peer_account = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = g.peer_pial;
DELETE FROM contact_grants WHERE owner_account IS NULL OR peer_account IS NULL;
ALTER TABLE contact_grants DROP CONSTRAINT contact_grants_owner_pial_peer_pial_key;
ALTER TABLE contact_grants DROP COLUMN peer_pial;
ALTER TABLE contact_grants ALTER COLUMN owner_account SET NOT NULL;
ALTER TABLE contact_grants ALTER COLUMN peer_account  SET NOT NULL;
ALTER TABLE contact_grants ADD CONSTRAINT contact_grants_account_pair UNIQUE (owner_account, peer_account);
DROP INDEX IF EXISTS idx_contact_grants_owner;
CREATE INDEX idx_contact_grants_owner_account ON contact_grants(owner_account) WHERE revoked_at IS NULL;

-- ── contact_requests: requester and recipient accounts ──────────────────────
ALTER TABLE contact_requests ADD COLUMN owner_account     UUID REFERENCES accounts(id);
ALTER TABLE contact_requests ADD COLUMN requester_account UUID REFERENCES accounts(id);
UPDATE contact_requests r SET owner_account = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = r.owner_pial;
UPDATE contact_requests r SET requester_account = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = r.requester_pial;
DELETE FROM contact_requests WHERE owner_account IS NULL OR requester_account IS NULL;
DROP INDEX IF EXISTS idx_contact_requests_pending;
DROP INDEX IF EXISTS idx_contact_requests_owner;
ALTER TABLE contact_requests DROP COLUMN requester_pial;
ALTER TABLE contact_requests ALTER COLUMN owner_account     SET NOT NULL;
ALTER TABLE contact_requests ALTER COLUMN requester_account SET NOT NULL;
CREATE UNIQUE INDEX idx_contact_requests_pending_accounts
    ON contact_requests(owner_account, requester_account) WHERE status = 'pending';
CREATE INDEX idx_contact_requests_owner_account
    ON contact_requests(owner_account, created_at DESC);

-- ── pial_messaging_keys: one key set per account ────────────────────────────
-- feed-engine provisions a messaging identity per ACCOUNT; the directory now
-- holds it per account too, so each persona's bundle carries its own keys.
-- Existing rows are the primary account's; another persona's key lands when it
-- next provisions.
ALTER TABLE pial_messaging_keys ADD COLUMN account_id UUID REFERENCES accounts(id);
UPDATE pial_messaging_keys k SET account_id = p.account_id
  FROM contact_primary_account p WHERE p.pial_id = k.pial_id;
DELETE FROM pial_messaging_keys WHERE account_id IS NULL;
ALTER TABLE pial_messaging_keys DROP CONSTRAINT pial_messaging_keys_pkey;
ALTER TABLE pial_messaging_keys ALTER COLUMN account_id SET NOT NULL;
ALTER TABLE pial_messaging_keys ADD PRIMARY KEY (account_id, device_id);
CREATE INDEX idx_messaging_keys_pial ON pial_messaging_keys(pial_id);

-- ── contact_address_accounts: the persona that minted an address ────────────
-- Addresses live in Manhattan and resolve to the identity node (the PIAL). This
-- brain records which account minted each one, so an address leads to that
-- persona's key bundle and is listed to that persona alone. An address minted
-- before this table existed has no row and is the PIAL's primary account's —
-- the same rule as the backfill above, applied where the address is read.
CREATE TABLE contact_address_accounts (
    address    TEXT        PRIMARY KEY,
    account_id UUID        NOT NULL REFERENCES accounts(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_contact_address_accounts_account ON contact_address_accounts(account_id);
