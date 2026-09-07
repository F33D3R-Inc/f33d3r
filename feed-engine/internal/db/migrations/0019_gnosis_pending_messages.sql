-- 0019_gnosis_pending_messages.sql
--
-- The message that did not get through.
--
-- Until now a conversation was the only place a message could exist, and a
-- conversation only came into existence after the contact policy had already
-- said yes. That ordering made one thing impossible: somebody writing to a
-- person who cannot yet be reached. The contact gate therefore had to be asked
-- for BEFORE anything was written — a knock placed on the recipient's behalf
-- for a message nobody had composed, and a refusal shown for a sentence that
-- was never typed.
--
-- This table is the missing state. It holds exactly one undelivered message per
-- (sender, target) pair: what was written, and what the contact authority last
-- said about it. It is the server's own account of the attempt, which is what
-- lets the gate render INSIDE the conversation with the composed message still
-- under it, and lets entering a Number send that message rather than asking the
-- person to type it again.
--
-- WHY THE SERVER HOLDS IT AND NOT THE BROWSER. A hidden form field would carry
-- the text back on the next request and cost no schema. It would also make the
-- browser the authority on what was written: the page could change it, lose it
-- on a reload, or replay it. Under Facet Architecture the server owns state and
-- the browser renders fragments, so the draft lives here and is rendered back
-- out. Nothing about the message is decided anywhere else.
--
-- WHY IT IS PLAINTEXT, SAID OUT LOUD. A first message to somebody you are not
-- yet permitted to reach cannot be end-to-end encrypted, because sealing needs
-- the recipient's key bundle and that bundle is precisely what contact
-- permission grants (/api/contact/{address}, /contact/keys). So a pending
-- message is server-readable by construction, exactly like a plain-mode
-- conversation, and storing it discloses nothing the request itself did not.
-- What it must never do is enter a SEALED conversation as plaintext: that would
-- be a silent downgrade of one message inside an encrypted thread. The delivery
-- path therefore delivers into plain conversations only, and renders the text
-- back into the sealed composer otherwise, where the client seals it.
--
-- THE RECIPIENT NEVER SEES IT BEFORE THEY AGREE. That is what makes a knock a
-- knock: the decision comes first and the message second. A contact request
-- carries an optional short NOTE saying why somebody is asking, and nothing
-- else; the body of this row is disclosed at exactly one moment, when the
-- conversation opens and it is written into it as an ordinary message.
--
-- ONE ROW PER PAIR. A person retrying is amending what they said, not queueing
-- a second attempt, so the unique constraint makes the retry an UPDATE and the
-- table cannot be grown without bound by anyone — nobody can stack a backlog
-- against a person who has not agreed to hear from them. The pair is also the
-- reason there is no conversation_id: this row exists only while no
-- conversation does.
--
-- AND IT DOES NOT WAIT FOR EVER. A message to somebody who never answers is
-- swept after gnosisPendingMaxAge (internal/cron), so an unanswered knock ages
-- out instead of sitting in the database indefinitely. Nothing is lost that the
-- sender has not already been told is undelivered.
--
-- REFERENTIAL ACTIONS, per the rule 0018 established: the sender is the subject
-- of their own undelivered message and the target is the only other party to
-- it, so an erasure on either side takes the row with it. Nothing here is a
-- record the platform keeps about someone; it is one unsent sentence.

CREATE TABLE IF NOT EXISTS gnosis_pending_messages (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  sender_account UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  target_account UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  body           TEXT        NOT NULL,
  -- What the contact authority last answered for this attempt, so re-opening
  -- the thread renders the same gate without asking again — and therefore
  -- without knocking on the recipient a second time. '' means never asked.
  -- 'request', 'deny' and 'unavailable' are the decision vocabulary this brain
  -- already speaks; 'allow' is never stored because an allowed message is
  -- delivered and the row is gone.
  last_decision  TEXT        NOT NULL DEFAULT ''
                 CHECK (last_decision IN ('', 'request', 'deny', 'unavailable')),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT gnosis_pending_messages_one_per_pair UNIQUE (sender_account, target_account),
  CONSTRAINT gnosis_pending_messages_not_self CHECK (sender_account <> target_account)
);

-- The two reads this table has: "what is this person still waiting to deliver"
-- (the pair, served by the unique constraint's index) and the id lookup on the
-- primary key. The target index exists for the erasure path, which deletes by
-- target and would otherwise sequentially scan.
CREATE INDEX IF NOT EXISTS idx_gnosis_pending_target
  ON gnosis_pending_messages(target_account);
