-- 0016_follow_counts_by_trigger.sql
--
-- user_profiles.follower_count / following_count were maintained by hand in
-- FollowUser and UnfollowUser. Those two functions were careful — one statement,
-- no half-apply — and they were still wrong, because they are not the only thing
-- that writes the follows table:
--
--     follows_follower_id_fkey  ... REFERENCES users(id) ON DELETE CASCADE
--     follows_following_id_fkey ... REFERENCES users(id) ON DELETE CASCADE
--
-- Deleting an account removes its follows rows inside the database, with no Go
-- function running and no counter moving. Measured drift before this migration:
-- @tehanibentley cached 20 followers against 11 real edges, entirely from
-- deleted accounts. An admin "resync follow counts" endpoint existed to repair
-- it, which is the tell: a number that needs a repair button is a number nobody
-- can trust between repairs.
--
-- A trigger cannot be bypassed. It fires in the same transaction as the row
-- change, for application writes, for cascades, and for anything written by hand
-- in psql. The counters stay (reads and ORDER BY stay index-fast) but nothing in
-- application code maintains them any more.

CREATE OR REPLACE FUNCTION follows_counters() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE user_profiles SET following_count = COALESCE(following_count, 0) + 1
         WHERE user_id = NEW.follower_id;
        UPDATE user_profiles SET follower_count  = COALESCE(follower_count,  0) + 1
         WHERE user_id = NEW.following_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE user_profiles SET following_count = GREATEST(0, COALESCE(following_count, 0) - 1)
         WHERE user_id = OLD.follower_id;
        UPDATE user_profiles SET follower_count  = GREATEST(0, COALESCE(follower_count,  0) - 1)
         WHERE user_id = OLD.following_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_follows_counters ON follows;
CREATE TRIGGER trg_follows_counters
    AFTER INSERT OR DELETE ON follows
    FOR EACH ROW EXECUTE FUNCTION follows_counters();

-- One-time reconciliation of the drift the trigger now makes impossible.
UPDATE user_profiles p SET
    follower_count  = (SELECT COUNT(*) FROM follows f WHERE f.following_id = p.user_id),
    following_count = (SELECT COUNT(*) FROM follows f WHERE f.follower_id  = p.user_id);
