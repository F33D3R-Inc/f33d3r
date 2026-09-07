package handler

// rail_budget.go — the right rail's one owner.
//
// THE DEFECT THIS FILE EXISTS TO END. The rail's length used to be an accident.
// Five panels chose their own size in three different files — trending asked the
// tag query for six, the two people panels each asked the suggestion owner for
// three, the news panel asked the RSS cache for four, and the scoreboard carried
// a constant of its own in sports.go — and nothing anywhere added them up. The
// rail was up to twenty-two rows long because nobody decided it should be
// twenty-two rows long. Worse, the next panel added would have grown it again
// silently, because there was no place a new panel had to go to be counted.
//
// So the fix is not a smaller number. The fix is that the rail has a budget, the
// budget is here, and a panel gets its size by asking for it. railCap panics on
// a panel that is not in the manifest, which means a new panel cannot render a
// row without first appearing below with a cap, an argument for that cap, and an
// answer for where a reader goes when the cap hides something. That is the whole
// point: the next panel's author has to make the decision the last five authors
// never had to make.
//
// WHAT A CAP IS. It is a height budget, not a fairness budget. The rail is a
// fixed-width column beside the Playground, it is not scrollable in its own
// right, and everything below the fold of it is decoration. A cap is the row
// count past which a glance becomes a scroll for that panel's row shape.
//
// WHAT A CAP IS NOT. It is not a filler quota. A panel that has fewer items than
// its cap renders fewer rows, and a panel that has none renders NOTHING — no
// head, no empty box. Two panels here rely on that and it must survive every
// change to this file: the scoreboard returns no dataset out of season, and the
// suggestion panel renders an honest empty line when nobody clears the quality
// bar rather than reaching further down the table for a row to fill space with.
// A cap that turned "nothing to show" into "empty box" would cost the rail more
// than an uncapped panel ever did.

// railPanelID names one panel of the rail. The set below is the rail: a panel
// that is not here does not have a share, and railCap will say so.
type railPanelID string

const (
	// railPanelTrending — "Trending": ranked hashtags.
	railPanelTrending railPanelID = "trending"
	// railPanelCreators — "Creators to follow": the one suggestion set narrowed
	// to creator accounts.
	railPanelCreators railPanelID = "creators_to_follow"
	// railPanelWhoToFollow — "Who to follow": the same suggestion set, minus
	// whoever the creator panel already spent.
	railPanelWhoToFollow railPanelID = "who_to_follow"
	// railPanelNews — "What's happening" / "Music news": syndicated headlines.
	railPanelNews railPanelID = "news"
	// railPanelScores — the cross-league scoreboard.
	railPanelScores railPanelID = "scores"
)

// railPanelDefaultCap is a panel's share unless the manifest argues for another
// one, and every panel but the scoreboard takes it unchanged.
//
// Three, because three is the length at which a panel is read rather than
// scanned. The rail is a stack of panels, not a page of one panel: a reader
// gives each head one look and each list about three rows before the next head
// takes over, and rows past that are paid for by the panel underneath rather
// than by the panel that printed them. Three also keeps a panel's head and its
// last row on the same screen at 1440px with every panel mounted, which is the
// difference between a rail and a second feed.
const railPanelDefaultCap = 3

// railItemBudget is the rail's ceiling: the most rows every panel together may
// print on one surface. It is the sum of the manifest, and railBudgetInvariants
// (called from init) refuses to start the process if it stops being the sum.
//
// It is written out as a number rather than computed so that adding a panel is
// a visible act. A future panel that takes three rows makes the manifest total
// twenty-one against a budget of eighteen, the process fails to start, and its
// author has to choose: raise this ceiling deliberately, or take the three rows
// from a panel that is no longer earning them. Either is a decision. Silently
// growing the rail is what this number makes impossible.
const railItemBudget = 18

// railPanelBudget is one panel's declared share of the rail.
type railPanelBudget struct {
	// ID is the panel this share belongs to.
	ID railPanelID

	// Cap is the most rows the panel may render.
	Cap int

	// Rationale is why this panel's rows cost what they cost, in this panel's
	// own row shape. A cap without one is a magic number with a comment field,
	// so the invariants refuse an empty Rationale.
	Rationale string

	// SeeAll is where a reader goes when the cap hides something — the panel's
	// escape hatch. It must be a route this service actually serves; the budget
	// test checks every value here against the registered route table, because a
	// capped panel whose "see more" 404s is worse than an uncapped panel.
	//
	// Empty means this panel HAS no escape hatch, which is a debt, not a
	// default — SeeAllNote must then say why the destination does not exist.
	SeeAll string

	// SeeAllNote explains a dynamic hatch, or the absence of one.
	SeeAllNote string
}

// railManifest is the rail's budget, panel by panel, in the order the rail
// renders them. This slice is the single definition the whole rail reads.
var railManifest = []railPanelBudget{
	{
		ID:  railPanelTrending,
		Cap: railPanelDefaultCap,
		Rationale: "Cut from six. Trending was the longest panel in the rail and " +
			"the least dense per row: a hashtag and a post count is the thinnest " +
			"row the rail prints, so the tail of it earned less per row of height " +
			"than anything below it, and six of them pushed the scoreboard and the " +
			"headlines under the fold on a laptop. Ranked lists also decay down the " +
			"list — the fourth, fifth and sixth tags are already the tags nobody " +
			"searched — so the rows cut are the rows worth least. The full grid on " +
			"/explore is one tap away and shows every tag with a card each.",
		SeeAll:     "/explore",
		SeeAllNote: "The Explore trending grid renders the same tags, unabridged.",
	},
	{
		ID:  railPanelCreators,
		Cap: railPanelDefaultCap,
		Rationale: "A creator row is an avatar, two lines of identity and a follower " +
			"count — the tallest people row in the rail. Three is also what the " +
			"suggestion owner's quality bar tends to yield for a fresh viewer; " +
			"asking for more mostly asks for people it would rather not name.",
		SeeAll: "",
		SeeAllNote: "NO ESCAPE HATCH, AND NO HONEST ONE EXISTS. There is no creator " +
			"directory to send a reader to. /search?type=people is the whole " +
			"suggestion set, not the creators-only narrowing this panel shows, so " +
			"pointing 'more creators' at it would promise a page that does not " +
			"exist. This stays blank until a creators surface does.",
	},
	{
		ID:  railPanelWhoToFollow,
		Cap: railPanelDefaultCap,
		Rationale: "The same row shape as the creator panel, one panel further down " +
			"the rail, and the second time this viewer is being asked the same " +
			"question. Three is already generous for a repeat ask.",
		SeeAll: "/search?type=people",
		SeeAllNote: "The People tab with nothing typed is this exact panel at twenty " +
			"rows: the same suggestion owner, the same quality bar, the same viewer " +
			"rules. It is the one destination that is genuinely 'more of this'.",
	},
	{
		ID:  railPanelNews,
		Cap: railPanelDefaultCap,
		Rationale: "Cut from four to the default. A headline row carries a 52px " +
			"thumbnail and up to three lines of clamped title, which makes it the " +
			"tallest row in the rail by a wide margin — four of them cost more " +
			"height than the six-row scoreboard. It is also the only panel whose " +
			"rows leave the platform, so it is the panel with the least claim on " +
			"height above the fold.",
		SeeAll: "",
		SeeAllNote: "NO ESCAPE HATCH, AND NO HONEST ONE EXISTS. The headlines are " +
			"syndicated and the platform serves no page that lists them; Explore's " +
			"News tab is a works feed, not this feed, and reaches no URL of its own. " +
			"Every row is already a link to the source. This stays blank until a " +
			"news index exists.",
	},
	{
		ID:  railPanelScores,
		Cap: 6,
		Rationale: "THE ONE EXCEPTION TO THE DEFAULT, AND IT IS ARGUED, NOT INHERITED. " +
			"Four was a single-league number. The panel now ranks every league at " +
			"once, purely by what is being played and what is on next, and it holds " +
			"no seat for anyone: on a Thursday in winter, four rows is a Thursday " +
			"football game and three basketball games, or the reverse, and an entire " +
			"sport disappears from the rail by arithmetic rather than by rank. Six " +
			"is the smallest number at which a two-league night cannot hide a " +
			"league. It buys that height cheaply — a scorecard is two lines with a " +
			"single right edge, the most compact row the rail prints, so six of them " +
			"stand roughly where six trending rows stood before this change. And it " +
			"is the one panel that already had a real way out: 'All scores' has been " +
			"in its head since it shipped. Read the ranking argument in full beside " +
			"sportsRailData in sports.go.",
		SeeAll: "",
		SeeAllNote: "HAS A HATCH, RESOLVED PER RENDER. sportsRailData writes HubHref " +
			"— the league's own board when every row is one league, the cross-league " +
			"board when they are mixed — and rail_scorecards renders it as 'All " +
			"scores'. It cannot be a fixed string here because the destination " +
			"depends on what is playing.",
	},
}

// railCap returns a panel's share of the rail.
//
// It panics on a panel that is not in the manifest, and that is the forcing
// function this whole file is for: the failure is a programming error, it is
// found the first time the rail renders, and the only way past it is to declare
// the panel's share above. A soft zero here would let a new panel render an
// empty list quietly, and a soft default would let it take rows nobody granted
// it — both are the drift this file ends.
func railCap(id railPanelID) int {
	for _, p := range railManifest {
		if p.ID == id {
			return p.Cap
		}
	}
	panic("rail budget: panel " + string(id) + " renders rows without a declared " +
		"share of the rail. Add it to railManifest in rail_budget.go with a cap, " +
		"the argument for that cap, and its escape hatch.")
}

// railSeeAll returns where a reader goes when a panel's cap hides something, or
// "" when the budget declares that this panel has no destination.
//
// The rail's facets render the link only when this is non-empty, so removing a
// hatch from the manifest removes it from the page, and a panel can never link
// to a route the budget did not name.
func railSeeAll(id railPanelID) string {
	for _, p := range railManifest {
		if p.ID == id {
			return p.SeeAll
		}
	}
	panic("rail budget: no escape hatch declared for panel " + string(id) +
		"; declare it in railManifest in rail_budget.go")
}

// railBudgetInvariants is what makes the budget a budget rather than a list.
//
// It runs at process start, so a manifest that no longer adds up stops the
// service here — at the one place that can explain why — instead of quietly
// producing a rail nobody sized.
func railBudgetInvariants() error {
	seen := make(map[railPanelID]bool, len(railManifest))
	total := 0
	for _, p := range railManifest {
		if p.ID == "" {
			return railBudgetError("a rail panel has no id")
		}
		if seen[p.ID] {
			return railBudgetError("rail panel " + string(p.ID) + " is declared twice; " +
				"one panel, one share")
		}
		seen[p.ID] = true
		if p.Cap < 1 {
			return railBudgetError("rail panel " + string(p.ID) + " has a cap below one; " +
				"a panel that renders nothing is removed, not budgeted at zero")
		}
		if p.Rationale == "" {
			return railBudgetError("rail panel " + string(p.ID) + " takes " +
				"rows without saying why; a cap with no argument is a magic number")
		}
		if p.SeeAll == "" && p.SeeAllNote == "" {
			return railBudgetError("rail panel " + string(p.ID) + " caps its list with " +
				"no escape hatch and no explanation; a capped panel that hides things " +
				"with no way to see more is worse than a long one, so say where the " +
				"reader goes or say why there is nowhere to send them")
		}
		total += p.Cap
	}
	if total != railItemBudget {
		return railBudgetError("the rail's panels add up to " + itoaRail(total) +
			" rows against a budget of " + itoaRail(railItemBudget) + ". Adding a panel " +
			"is a decision about the whole rail: either raise railItemBudget " +
			"deliberately, or take the rows from a panel that has stopped earning them.")
	}
	return nil
}

// railBudgetError is the error type the invariants return, named so a startup
// failure reads as what it is.
type railBudgetError string

func (e railBudgetError) Error() string { return "rail budget: " + string(e) }

// itoaRail renders a small non-negative count without pulling strconv into a
// file whose only numbers are row counts.
func itoaRail(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func init() {
	if err := railBudgetInvariants(); err != nil {
		panic(err)
	}
}
