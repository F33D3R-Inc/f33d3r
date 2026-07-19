package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/model"
)

type achDef struct {
	id, name, desc, icon, category, rarity string
	xpReward                               int
	earned                                 func(*model.User) bool
}

var achievementCatalog = []achDef{
	// ONBOARDING
	{"first_pulse", "First Pulse", "Created account and entered F33D3R for the first time.", "💓", "onboarding", "common", 50, func(u *model.User) bool { return true }},
	{"signal_found", "Signal Found", "Completed profile setup.", "📡", "onboarding", "common", 100, func(u *model.User) bool { return u.Bio != "" }},
	{"face_reveal", "Face Reveal", "Uploaded first profile photo or avatar.", "🪞", "onboarding", "common", 75, func(u *model.User) bool { return u.AvatarURL != "" }},
	{"echo_online", "Echo Online", "Verified email or phone.", "✅", "onboarding", "common", 100, func(u *model.User) bool { return u.IsVerified }},
	{"wanderer", "Wanderer", "Returned to F33D3R 7 days in a row.", "🚶", "onboarding", "uncommon", 200, func(u *model.User) bool { return false }},
	// SOCIAL
	{"first_contact", "First Contact", "Sent first message.", "📩", "social", "common", 50, func(u *model.User) bool { return false }},
	{"crowd_surfer", "Crowd Surfer", "Received 100 total reactions.", "🏄", "social", "uncommon", 300, func(u *model.User) bool { return false }},
	{"main_character", "Main Character", "First post breaks platform engagement threshold.", "⭐", "social", "rare", 500, func(u *model.User) bool { return false }},
	{"summoned", "Summoned", "Tagged by another user for the first time.", "👆", "social", "common", 100, func(u *model.User) bool { return false }},
	{"algorithm_knows_you", "The Algorithm Knows You", "Appeared on trending or discovery feed.", "🤖", "social", "rare", 750, func(u *model.User) bool { return false }},
	{"ratiod", "Ratio'd", "Received more replies than likes on a post.", "📊", "social", "uncommon", 150, func(u *model.User) bool { return false }},
	{"villain_arc", "Villain Arc", "Lost major follower count in 24 hours.", "😈", "social", "uncommon", 200, func(u *model.User) bool { return false }},
	{"celebrity_sighting", "Celebrity Sighting", "Interacted with verified or high-rank account.", "🌟", "social", "uncommon", 250, func(u *model.User) bool { return false }},
	{"popular", "Popular", "Gained 100 followers.", "🌟", "social", "uncommon", 200, func(u *model.User) bool { return u.FollowerCount >= 100 }},
	{"celebrity", "Celebrity", "Gained 1,000 followers.", "💫", "social", "rare", 500, func(u *model.User) bool { return u.FollowerCount >= 1000 }},
	{"influencer", "Influencer", "Gained 10,000 followers.", "🚀", "social", "epic", 1500, func(u *model.User) bool { return u.FollowerCount >= 10000 }},
	// CREATOR
	{"mic_check", "Mic Check", "Uploaded first audio or song.", "🎤", "creator", "common", 150, func(u *model.User) bool { return false }},
	{"directors_cut", "Director's Cut", "Uploaded first video.", "🎬", "creator", "common", 150, func(u *model.User) bool { return false }},
	{"first_sale", "First Sale", "Sold first digital item or content.", "💰", "creator", "uncommon", 500, func(u *model.User) bool { return false }},
	{"paid_creator", "Paid Creator", "Earned first payout.", "💸", "creator", "uncommon", 750, func(u *model.User) bool { return false }},
	{"independent_artist", "Independent Artist", "Reached monetization threshold without sponsors.", "🎨", "creator", "rare", 1000, func(u *model.User) bool { return false }},
	{"cult_following", "Cult Following", "Maintained high repeat audience ratio.", "🔮", "creator", "rare", 1000, func(u *model.User) bool { return false }},
	{"studio_session", "Studio Session", "Uploaded content consistently for 30 days.", "🎵", "creator", "rare", 1500, func(u *model.User) bool { return false }},
	{"world_tour", "World Tour", "Content viewed in 10 or more countries.", "🌍", "creator", "epic", 2000, func(u *model.User) bool { return false }},
	// MUSIC
	{"bedroom_producer", "Bedroom Producer", "Published first song.", "🎹", "music", "common", 200, func(u *model.User) bool { return false }},
	{"aux_cord_certified", "Aux Cord Certified", "Song replayed repeatedly by listeners.", "🔁", "music", "uncommon", 400, func(u *model.User) bool { return false }},
	{"local_legend", "Local Legend", "Trending in one city or region.", "📍", "music", "rare", 800, func(u *model.User) bool { return false }},
	{"headliner", "Headliner", "Hit major platform stream milestone.", "🎪", "music", "epic", 2500, func(u *model.User) bool { return false }},
	{"soundtrack_of_summer", "Soundtrack of Summer", "Seasonal viral music trend achievement.", "☀️", "music", "legendary", 5000, func(u *model.User) bool { return false }},
	// WELLNESS
	{"touch_grass", "Touch Grass", "Reduced screen time for consecutive days.", "🌿", "wellness", "uncommon", 300, func(u *model.User) bool { return false }},
	{"logged_off", "Logged Off", "Stayed off platform for a healthy duration.", "🛌", "wellness", "common", 100, func(u *model.User) bool { return false }},
	{"balance_patch", "Balance Patch", "Maintained healthy online/offline ratio.", "⚖️", "wellness", "uncommon", 400, func(u *model.User) bool { return false }},
	{"night_shift", "Night Shift", "Used platform during late-night hours consistently.", "🌙", "wellness", "common", 150, func(u *model.User) bool { return false }},
	{"doomscroll_survivor", "Doomscroll Survivor", "Exceeded unhealthy scrolling threshold.", "📱", "wellness", "common", 100, func(u *model.User) bool { return false }},
	{"digital_detox", "Digital Detox", "Took voluntary cooldown from app.", "🧘", "wellness", "rare", 500, func(u *model.User) bool { return false }},
	// CULTURE
	{"curiosity_killed_cat", "Curiosity Killed the Cat", "Entered adult content section for first time.", "🐱", "culture", "common", 100, func(u *model.User) bool { return false }},
	{"after_dark", "After Dark", "Active in adult content after midnight.", "🌑", "culture", "common", 100, func(u *model.User) bool { return false }},
	{"incognito_failure", "Incognito Failure", "Returned repeatedly to same creator or category.", "🕵️", "culture", "uncommon", 200, func(u *model.User) bool { return false }},
	{"bonk", "Bonk", "Triggered platform content detection systems.", "🔨", "culture", "uncommon", 150, func(u *model.User) bool { return false }},
	{"post_nut_clarity", "Post-Nut Clarity", "Immediately logged off after adult session.", "💭", "culture", "uncommon", 200, func(u *model.User) bool { return false }},
	{"down_astronomical", "Down Astronomical", "Excessive late-night time in adult content.", "📉", "culture", "uncommon", 200, func(u *model.User) bool { return false }},
	{"grass_avoider", "Grass Avoider", "Excessive consecutive adult-content browsing session.", "🪴", "culture", "uncommon", 200, func(u *model.User) bool { return false }},
	{"chronically_online", "Chronically Online", "Extreme weekly activity.", "💻", "culture", "common", 200, func(u *model.User) bool { return false }},
	{"keyboard_warrior", "Keyboard Warrior", "Entered excessive debate threads.", "⌨️", "culture", "common", 150, func(u *model.User) bool { return false }},
	{"deleted_in_5", "Deleted in 5 Minutes", "Removed embarrassing post quickly.", "🗑️", "culture", "common", 100, func(u *model.User) bool { return false }},
	{"screenshot_immortalized", "Screenshot Immortalized", "Post archived or shared widely.", "📸", "culture", "rare", 750, func(u *model.User) bool { return false }},
	{"the_receipts", "The Receipts", "Uploaded proof during drama or conflict.", "🧾", "culture", "uncommon", 300, func(u *model.User) bool { return false }},
	{"npc_behavior", "NPC Behavior", "Repeated identical interactions excessively.", "🤖", "culture", "uncommon", 200, func(u *model.User) bool { return false }},
	{"lore_drop", "Lore Drop", "Revealed major personal backstory.", "📖", "culture", "uncommon", 350, func(u *model.User) bool { return false }},
	// REAL LIFE
	{"survivor", "Survivor", "Documented surviving natural disaster or crisis.", "🌊", "real_life", "epic", 2000, func(u *model.User) bool { return false }},
	{"builder", "Builder", "Started verified business or project.", "🏗️", "real_life", "rare", 1000, func(u *model.User) bool { return u.IsCreator }},
	{"graduate", "Graduate", "Completed educational milestone.", "🎓", "real_life", "uncommon", 500, func(u *model.User) bool { return false }},
	{"passport_stamped", "Passport Stamped", "Verified international travel milestone.", "✈️", "real_life", "uncommon", 400, func(u *model.User) bool { return false }},
	// COMMUNITY
	{"founding_member", "Founding Member", "Early adopter before public growth.", "🏛️", "community", "legendary", 5000, func(u *model.User) bool { return false }},
	{"guildmaster", "Guildmaster", "Owns successful community or group.", "👑", "community", "epic", 2000, func(u *model.User) bool { return false }},
	{"raid_leader", "Raid Leader", "Organized large event, stream, or community push.", "⚔️", "community", "epic", 1500, func(u *model.User) bool { return false }},
	{"peacekeeper", "Peacekeeper", "Resolved moderation disputes successfully.", "🕊️", "community", "rare", 1000, func(u *model.User) bool { return false }},
	// ECONOMY
	{"bread_winner", "Bread Winner", "Earned first real income on platform.", "🍞", "economy", "uncommon", 500, func(u *model.User) bool { return false }},
	{"whale", "Whale", "Spent large amount supporting creators.", "🐳", "economy", "rare", 1000, func(u *model.User) bool { return false }},
	{"tip_jar_hero", "Tip Jar Hero", "Supported many small creators.", "💝", "economy", "uncommon", 400, func(u *model.User) bool { return false }},
	{"hustler", "Hustler", "Maintained monthly monetization streak.", "💼", "economy", "rare", 1500, func(u *model.User) bool { return false }},
	{"black_market_energy", "Black Market Energy", "Frequently traded rare digital items.", "🖤", "economy", "epic", 2000, func(u *model.User) bool { return false }},
	// SECRET (shown only if earned)
	{"ghost_in_machine", "Ghost in the Machine", "Triggered hidden system event.", "👻", "secret", "legendary", 5000, func(u *model.User) bool { return false }},
	{"chosen_one", "Chosen One", "Extremely low unlock rate achievement.", "⚡", "secret", "mythic", 10000, func(u *model.User) bool { return false }},
	{"forbidden_knowledge", "Forbidden Knowledge", "Found hidden feature or page.", "📚", "secret", "legendary", 5000, func(u *model.User) bool { return false }},
	{"developer_is_watching", "Developer Is Watching", "Direct interaction with official staff or system.", "👁️", "secret", "legendary", 7500, func(u *model.User) bool { return false }},
	// TROLL — Psychological Warfare
	{"troll_master_baiter",           "Master Baiter",           "Successfully bait 100 users into replying.",                    "🪝",  "troll", "silver",   750,  func(u *model.User) bool { return false }},
	{"troll_gaslight_specialist",     "Gaslight Specialist",     "Made 50+ replies in threads you did not start.",                "🕯️", "troll", "silver",   750,  func(u *model.User) bool { return false }},
	{"troll_professional_instigator", "Professional Instigator", "Start drama that reaches trending.",                            "📢",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_villain_arc_v2",          "The Villain Arc",         "Lost 500 followers and gained 1,000 in the same week.",        "😈",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_thread_hijacker",         "Thread Hijacker",         "Become the top comment on someone else's viral post.",         "🧵",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_public_execution",        "Public Execution",        "Ratio someone by 10x.",                                        "⚰️", "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_demon_time",              "Demon Time",              "Cause 1,000 replies in under an hour.",                        "😈",  "troll", "platinum", 5000, func(u *model.User) bool { return false }},
	{"troll_emotional_damage",        "Emotional Damage",        "Someone leaves the platform after your post.",                 "💔",  "troll", "platinum", 5000, func(u *model.User) bool { return false }},
	{"troll_nuclear_take",            "Nuclear Take",            "Post something with 90% negative reactions and survive.",      "☢️", "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_fearless",                "Fearless",                "Keep posting while everyone wants you banned.",                 "🗿",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	// TROLL — Elite Schizo Poster
	{"troll_off_the_meds",            "Off The Meds",            "Post 100 times between 2AM–5AM.",                              "🌙",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_wall_of_text",            "Wall Of Text Warrior",    "Write a 5,000 character post.",                                "📜",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_conspiracy_dept",         "Conspiracy Department",   "Posted 50 times with hashtag obsession.",                      "🔍",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_unemployed_final_boss",   "Unemployed Final Boss",   "16 straight hours active.",                                    "💻",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_shadow_government",       "Shadow Government",       "10+ accounts followed you in the same hour.",                  "🏛️", "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_paranoid_legend",         "Paranoid Legend",         "Accused 100 users of being bots.",                             "🤖",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_hallucination_engine",    "Hallucination Engine",    "Your post draws 50+ replies from people who've never engaged you before.", "👻", "troll", "gold", 2000, func(u *model.User) bool { return false }},
	// TROLL — Reputation-Based
	{"troll_most_hated",              "Most Hated",              "Reach top 1% of most-blocked users.",                          "🖤",  "troll", "platinum", 5000, func(u *model.User) bool { return false }},
	{"troll_mute_magnet",             "Mute Magnet",             "Muted by 5,000 people.",                                       "🔇",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_community_threat",        "Community Threat",        "Reported 1,000 times.",                                        "⚠️", "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_digital_herpes",          "Digital Herpes",          "Your posts keep reappearing everywhere — 500 total reposts.",  "🔁",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_known_terrorist",         "Known Terrorist",         "Your profile has been visited 50,000 times.",                  "💀",  "troll", "platinum", 5000, func(u *model.User) bool { return false }},
	{"troll_blacklist_material",      "Blacklist Material",      "10+ of your posts removed by moderation.",                     "🚫",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_walking_disaster",        "Walking Disaster",        "200+ reports filed against your account.",                     "🌀",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	// TROLL — True Troll Milestones
	{"troll_i_own_you",               "I Own You",               "Someone writes a 500+ character reply to your post.",          "🧠",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_rent_free",               "Rent Free",               "Someone you've never talked to mentions you.",                 "🏠",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_crashout_inducer",        "Crashout Inducer",        "A thread you started reaches 50+ replies.",                    "💥",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_clip_farm",               "The Clip Farm",           "A single post of yours is reposted 50+ times.",                "📹",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_reverse_ratio",           "Reverse Ratio",           "A post of yours has 100+ dislikes and 100+ likes.",            "📊",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_infinite_aura_loss",      "Infinite Aura Loss",      "One of your replies gets 100+ dislikes.",                      "📉",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_feigning_ignorance",      "Feigning Ignorance",      "Post 20+ replies in a single thread.",                         "🙈",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_goalpost_olympics",       "Goalpost Olympics",       "Post 15+ replies in a single thread.",                         "🥅",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_bad_faith_actor",         "Bad Faith Actor",         "Post 10+ replies in a single thread.",                         "🎭",  "troll", "silver",   300,  func(u *model.User) bool { return false }},
	// TROLL — South Park Internet
	{"troll_cartman_method",          "Cartman Method",          "Get 50+ unique users into a single thread you started.",      "🎬",  "troll", "gold",     2000, func(u *model.User) bool { return false }},
	{"troll_its_just_a_joke",         "It's Just A Joke Bro",   "Reported 10+ times in a week with no action taken.",           "😂",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	{"troll_uncancelable",            "Uncancelable",            "Survived 100+ reports without suspension.",                    "🛡️", "troll", "platinum", 3000, func(u *model.User) bool { return false }},
	{"troll_moral_bankruptcy",        "Moral Bankruptcy",        "Blocked by 1,000+ and reported 500+ times.",                  "💸",  "troll", "platinum", 5000, func(u *model.User) bool { return false }},
	{"troll_peak_2004_internet",      "Peak 2004 Internet",      "Reported 200+ times, never banned.",                          "🌐",  "troll", "platinum", 3000, func(u *model.User) bool { return false }},
	{"troll_weaponized_autism",       "Weaponized Autism",       "Reply to a post that is 2+ years old.",                       "📅",  "troll", "uncommon", 250,  func(u *model.User) bool { return false }},
	{"troll_forum_demon",             "Forum Demon",             "Reply in 10+ trending threads in a single day.",               "😈",  "troll", "gold",     1500, func(u *model.User) bool { return false }},
	{"troll_archive_diver",           "Archive Diver",           "Quote or reply to a post that is 1+ year old.",               "🗄️", "troll", "silver",   300,  func(u *model.User) bool { return false }},
	{"troll_digital_cigarette",       "Digital Cigarette Smoker","Post 50+ replies between 11PM–4AM.",                          "🚬",  "troll", "silver",   500,  func(u *model.User) bool { return false }},
	// TROLL — Legendary
	{"troll_final_boss",              "Final Boss Of The Timeline","Your post reaches 10,000+ interactions.",                   "👑",  "troll", "platinum", 7500, func(u *model.User) bool { return false }},
	{"troll_internet_antichrist",     "Internet Antichrist",     "Reach maximum negative reputation — blocked and reported by the masses.", "😈", "troll", "platinum", 7500, func(u *model.User) bool { return false }},
	{"troll_reason_we_need_rules",    "The Reason We Need Rules","Admin-granted. You caused a policy change.",                  "📋",  "troll", "platinum", 10000, func(u *model.User) bool { return false }},
	{"troll_sitewide_event",          "Sitewide Event",          "A single post of yours reaches 50,000 impressions.",          "🌍",  "troll", "platinum", 7500, func(u *model.User) bool { return false }},
	{"troll_everybody_saw_it",        "Everybody Saw It",        "A post of yours reaches 100,000 impressions.",                "👁️", "troll", "platinum", 10000, func(u *model.User) bool { return false }},
	{"troll_chaos_god",               "Chaos God",               "Post goes viral both positively and negatively — 500+ likes and 500+ dislikes.", "⚡", "troll", "platinum", 10000, func(u *model.User) bool { return false }},
	{"troll_main_villain",            "The Main Villain",        "Most blocked user on the platform for any day.",               "🦹",  "troll", "platinum", 7500, func(u *model.User) bool { return false }},
	{"troll_historically_problematic","Historically Problematic","Mentioned by 100+ unique users in 30 days.",                 "📰",  "troll", "platinum", 7500, func(u *model.User) bool { return false }},
	{"troll_untouchable",             "Untouchable",             "Survived 500+ reports without a single ban.",                  "💎",  "troll", "platinum", 10000, func(u *model.User) bool { return false }},
	{"troll_south_park_s4",           "South Park Season 4",     "Unlocked every troll achievement.",                           "🏆",  "troll", "mythic",   25000, func(u *model.User) bool { return false }},
}

// xpTierFor returns tier name and XP-to-next for a given total XP.
func xpTierFor(xp int) (tier, next string, toNext, pct int) {
	tiers := [][2]interface{}{
		{"Civilian", 0}, {"Creator", 500}, {"Merchant", 1500}, {"Scout", 4000},
		{"Journalist", 8000}, {"Archivist", 15000}, {"Broadcaster", 25000},
		{"Veteran", 50000}, {"Elite", 100000}, {"Mythic", 250000},
	}
	tier = "Civilian"
	for i, t := range tiers {
		if xp >= t[1].(int) {
			tier = t[0].(string)
			if i+1 < len(tiers) {
				next = tiers[i+1][0].(string)
				floor := t[1].(int)
				ceil := tiers[i+1][1].(int)
				toNext = ceil - xp
				r := ceil - floor
				if r > 0 {
					pct = (xp-floor)*100/r
					if pct > 100 {
						pct = 100
					}
				}
			} else {
				next = "Mythic"
				pct = 100
			}
		}
	}
	return
}

type achGroup struct {
	Slug         string
	Label        string
	Achievements []model.Achievement
	EarnedCount  int
}

var achCategoryOrder = []struct{ slug, label string }{
	{"onboarding", "Getting Started"},
	{"social", "Social"},
	{"troll", "Troll"},
	{"creator", "Creator"},
	{"music", "Music"},
	{"wellness", "Wellness"},
	{"culture", "Culture"},
	{"real_life", "Real Life"},
	{"community", "Community"},
	{"economy", "Economy"},
	{"secret", "Secret"},
}

func (h *Handler) achievementsPage(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)

	now := time.Now()
	earnedCount := 0
	totalXP := 0

	byCategory := make(map[string][]model.Achievement)
	for _, d := range achievementCatalog {
		a := model.Achievement{
			ID:          d.id,
			Name:        d.name,
			Description: d.desc,
			Icon:        d.icon,
			Category:    d.category,
			Rarity:      d.rarity,
			XPReward:    d.xpReward,
			Tier:        d.rarity,
		}
		if d.earned(user) {
			a.EarnedAt = &now
			earnedCount++
			totalXP += d.xpReward
		}
		byCategory[d.category] = append(byCategory[d.category], a)
	}

	groups := make([]achGroup, 0, len(achCategoryOrder))
	for _, cat := range achCategoryOrder {
		achs := byCategory[cat.slug]
		if len(achs) == 0 {
			continue
		}
		earned := 0
		for _, a := range achs {
			if a.EarnedAt != nil {
				earned++
			}
		}
		groups = append(groups, achGroup{
			Slug:         cat.slug,
			Label:        cat.label,
			Achievements: achs,
			EarnedCount:  earned,
		})
	}

	tier, next, toNext, pct := xpTierFor(totalXP)
	rail := h.railData(user, "default")

	// Leaderboard data — fetch viewer's country then all four boards.
	var countryCode string
	if user != nil && h.db != nil {
		h.db.QueryRow(`SELECT COALESCE(country_code,'') FROM user_profiles WHERE user_id=$1`, user.ID).Scan(&countryCode)
	}
	dailyGlobal, _ := dbpkg.GetGlobalLeaderboard(h.db)
	var dailyCountry []model.LeaderboardEntry
	if countryCode != "" {
		dailyCountry, _ = dbpkg.GetCountryLeaderboard(h.db, countryCode)
	}
	allTimeGlobal, _ := dbpkg.GetGlobalAllTimeLeaderboard(h.db)
	var allTimeCountry []model.LeaderboardEntry
	if countryCode != "" {
		allTimeCountry, _ = dbpkg.GetCountryAllTimeLeaderboard(h.db, countryCode)
	}
	var myRank int
	var myXPToday int64
	inTop10 := false
	if user != nil && h.db != nil {
		myRank, myXPToday, _ = dbpkg.GetUserDailyRank(h.db, user.ID)
		for _, e := range dailyGlobal {
			if e.UserID == user.ID {
				inTop10 = true
				break
			}
		}
	}

	h.render(w, "achievements.html", map[string]interface{}{
		"User":              user,
		"AchievementGroups": groups,
		"EarnedCount":       earnedCount,
		"TotalCount":        len(achievementCatalog),
		"XPTotal":           totalXP,
		"XPPercent":         pct,
		"CurrentTier":       tier,
		"CurrentTierLower":  strings.ToLower(tier),
		"NextTier":          next,
		"XPToNext":          toNext,
		"Title":             "Achievements · F33D3R",
		"SessionID":         uuid.New().String(),
		"ShowScores":        h.cfg.ShowScores,
		"Themes":            ThemesWithActive(user.ThemeID),
		"TrendingTags":      rail["TrendingTags"],
		"SuggestedUsers":    rail["SuggestedUsers"],
		"RailContext":       rail["RailContext"],
		"RailNewsItems":     rail["RailNewsItems"],
		"RailNewsLabel":     rail["RailNewsLabel"],
		"DailyGlobalBoard":  dailyGlobal,
		"DailyCountryBoard": dailyCountry,
		"AllTimeGlobalBoard":  allTimeGlobal,
		"AllTimeCountryBoard": allTimeCountry,
		"CountryName":       countryName(countryCode),
		"MyRank":            myRank,
		"MyXPToday":         myXPToday,
		"InTop10":           inTop10,
	})
}

// facetLeaderboardPanel serves GET /facets/leaderboard_panel.
// Returns the full four-board leaderboard panel as an HTML fragment.
func (h *Handler) facetLeaderboardPanel(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if h.db == nil || h.partial == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}

	var countryCode string
	if user != nil && user.ID != "" {
		h.db.QueryRow(`SELECT COALESCE(country_code,'') FROM user_profiles WHERE user_id=$1`, user.ID).Scan(&countryCode)
	}

	dailyGlobal, _ := dbpkg.GetGlobalLeaderboard(h.db)
	var dailyCountry []model.LeaderboardEntry
	if countryCode != "" {
		dailyCountry, _ = dbpkg.GetCountryLeaderboard(h.db, countryCode)
	}
	allTimeGlobal, _ := dbpkg.GetGlobalAllTimeLeaderboard(h.db)
	var allTimeCountry []model.LeaderboardEntry
	if countryCode != "" {
		allTimeCountry, _ = dbpkg.GetCountryAllTimeLeaderboard(h.db, countryCode)
	}

	var myRank int
	var myXPToday int64
	if user != nil && user.ID != "" {
		myRank, myXPToday, _ = dbpkg.GetUserDailyRank(h.db, user.ID)
	}

	inTop10 := false
	for _, e := range dailyGlobal {
		if user != nil && e.UserID == user.ID {
			inTop10 = true
			break
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "leaderboard_panel", map[string]interface{}{
		"User":                user,
		"DailyGlobalBoard":    dailyGlobal,
		"DailyCountryBoard":   dailyCountry,
		"AllTimeGlobalBoard":  allTimeGlobal,
		"AllTimeCountryBoard": allTimeCountry,
		"CountryName":         countryName(countryCode),
		"MyRank":              myRank,
		"MyXPToday":           myXPToday,
		"InTop10":             inTop10,
	})
}

// broadcastLeaderboardRefresh signals all active sessions to refresh their
// leaderboard panel. Called after XP is awarded so the board updates live.
func (h *Handler) broadcastLeaderboardRefresh() {
	PublishToAllSessions(SSEEvent{Type: "leaderboard_refresh", Data: "1"})
}

// facetXPBar serves GET /facets/xp_bar — returns a fresh xp_bar partial.
// Called by client-side SSE handler on leaderboard_refresh so the sidebar bar
// updates without a full page reload.
func (h *Handler) facetXPBar(w http.ResponseWriter, r *http.Request) {
	user := h.userFromRequest(w, r)
	if user == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.renderPartial(w, "xp_bar", map[string]interface{}{"User": user})
}
