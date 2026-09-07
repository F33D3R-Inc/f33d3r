-- 0013_interest_surface_bridge.sql
--
-- The interest surfaces users pin to their tab bar are defined by a tag set and
-- a content type in feed_surfaces. Until now nothing read that definition — every
-- pinned surface fell through to the Following feed — so the tag sets were never
-- exercised against real works and are too narrow to select any.
--
-- Two facts about the data they have to match:
--
--   * Authors write league and scene names, not category names. A game thread is
--     tagged #NFL, not #sports. A surface whose only sports word is "sports"
--     selects nothing on a Sunday.
--   * Tags are stored with the author's own casing. The read side lowercases both
--     sides (GetWorksBySurface), so these stay lowercase.
--
-- Widening the vocabulary is additive: every tag already listed is kept, so no
-- work that matched a surface before stops matching it now.

UPDATE feed_surfaces SET tags = ARRAY[
    'sports','football','basketball','fitness',
    'nfl','nba','mlb','nhl','ncaa','soccer','futbol','premierleague','ufc','mma',
    'boxing','f1','formula1','tennis','golf','cricket','rugby','olympics',
    'baseball','hockey','superbowl','playoffs','gameday','scores'
]::text[] WHERE id = 'sports';

UPDATE feed_surfaces SET tags = ARRAY[
    'art','digitalart','illustration','design','painting','drawing','sketch',
    'artist','concept art','3d','animation','photography'
]::text[] WHERE id = 'art';

UPDATE feed_surfaces SET tags = ARRAY[
    'gaming','games','gamer','esports','twitch','speedrun','indiegame','fps',
    'rpg','minecraft','fortnite','valorant','league of legends'
]::text[] WHERE id = 'gaming';

UPDATE feed_surfaces SET tags = ARRAY[
    'tech','coding','ai','programming','dev','software','opensource','rust','go',
    'golang','linux','devops','security','infosec','startup'
]::text[] WHERE id = 'tech';

UPDATE feed_surfaces SET tags = ARRAY[
    'fashion','style','ootd','streetwear','runway','vintage','thrift','makeup',
    'beauty','skincare'
]::text[] WHERE id = 'fashion';

UPDATE feed_surfaces SET tags = ARRAY[
    'food','cooking','recipe','foodie','baking','chef','restaurant','vegan',
    'bbq','coffee'
]::text[] WHERE id = 'food';

UPDATE feed_surfaces SET tags = ARRAY[
    'crypto','web3','blockchain','nft','defi','bitcoin','btc','xrp','ethereum',
    'eth','mining','wallet'
]::text[] WHERE id = 'crypto';

UPDATE feed_surfaces SET tags = ARRAY[
    'film','movies','cinema','tv','series','a24','horror','scifi','documentary',
    'anime','netflix'
]::text[] WHERE id = 'film';

UPDATE feed_surfaces SET tags = ARRAY[
    'books','reading','literature','author','poetry','writing','bookstagram',
    'fiction','nonfiction','manga'
]::text[] WHERE id = 'books';

UPDATE feed_surfaces SET tags = ARRAY[
    'travel','wanderlust','adventure','explore','roadtrip','hiking','backpacking',
    'nature','citybreak'
]::text[] WHERE id = 'travel';

-- Music and Video are content_type surfaces (audio / video). They also carry the
-- words people actually tag, so a text work about a release reaches them too.
UPDATE feed_surfaces SET tags = ARRAY[
    'music','song','album','producer','beats','hiphop','rap','rnb','edm','dj',
    'guitar','vinyl','newmusic'
]::text[] WHERE id = 'music';

UPDATE feed_surfaces SET tags = ARRAY[
    'video','shortfilm','vlog','edit','reels','filmmaking','trailer'
]::text[] WHERE id = 'video';
