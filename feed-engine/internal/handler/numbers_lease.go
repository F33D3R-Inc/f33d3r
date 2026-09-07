package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ── The lease on a Number ────────────────────────────────────────────────────
//
// A Number carries three owner-owned properties beyond its policy: a LABEL that
// says which one it is, an EXPIRY that ends it without anybody remembering, and
// a USE BUDGET that bounds how many strangers it introduces.
//
// None of them are decided here. elohim-veni owns the columns, the bounds and
// the enforcement; Manhattan owns the Number itself and, by its own doctrine, no
// payload at all. This file is the rendering surface: it draws the vocabulary
// the owner chooses from, reads the choice off the form, and turns the
// authority's answer into words. It decides nothing.
//
// Nothing in this file is ever reachable from a resolve. A label, a remaining
// budget and a time left are facts about the owner's own Number, and the
// resolver is told none of them — not in a fragment, not in an error, and not
// by the clock.

// numberExpiryOption is one entry in the expiry vocabulary the owner picks from.
//
// Every preset is a DURATION, never a date. A date would have to be read in the
// browser's timezone against the browser's clock and then reconciled with the
// authority's — three clocks for one deadline. A duration has one meaning
// everywhere and becomes an instant exactly once, in elohim-veni, against the
// same database clock that later enforces it.
type numberExpiryOption struct {
	// Value is a whole number of seconds, "" for no expiry, or the sentinel
	// "custom", which reveals the day count field.
	Value string
	Label string
}

// numberExpiryCustom is the sentinel that means "read expiry_days instead".
const numberExpiryCustom = "custom"

// numberExpiryKeep is the sentinel that means "the owner did not touch this".
//
// A Number's expiry is stored as an INSTANT, so a select cannot pre-select the
// owner's current choice — "in six days" is not one of the options they picked
// from. Without an explicit way to say nothing, the control would default to
// "No expiry" and an owner who came to change the budget would silently discard
// the lease. This is that way of saying nothing, and it is the default on every
// form that edits a Number that already exists.
const numberExpiryKeep = "keep"

// numberExpiryDaysMax bounds the day count the form will offer. It is a hint
// drawn in the input, not a rule: the rule lives in elohim-veni, which owns the
// column, and a second copy of a bound is a second answer waiting to disagree.
const numberExpiryDaysMax = 365

// numberExpiryOptions is the vocabulary, in the order it is rendered.
//
// The presets are the owner's own use cases, in their words: "a project test
// group or at a conference or a class or other group or one on one meet ups."
// A meet-up is a day. A conference is a long weekend or a week. A class or a
// campaign is a month. A project is a term.
//
// No expiry is FIRST and is the default, deliberately. A Number that quietly
// stopped working because a default nobody chose ran out would be a worse
// failure than the one this feature exists to fix, and every Number that
// already exists keeps the promise it was minted under.
func numberExpiryOptions() []numberExpiryOption {
	return []numberExpiryOption{
		{Value: "", Label: "No expiry"},
		{Value: "86400", Label: "1 day — a meet-up"},
		{Value: "259200", Label: "3 days — a conference"},
		{Value: "604800", Label: "1 week — an event or a workshop"},
		{Value: "2592000", Label: "30 days — a class or a campaign"},
		{Value: "7776000", Label: "90 days — a project or a term"},
		{Value: numberExpiryCustom, Label: "A number of days…"},
	}
}

// numberExpiryChangeOptions is the vocabulary for a Number that already exists.
//
// It is the mint vocabulary with "leave unchanged" in front and selected. An
// expiry is stored as an instant, so nothing here can pre-select what the owner
// already chose; the honest default is therefore to change nothing.
func numberExpiryChangeOptions() []numberExpiryOption {
	return append(
		[]numberExpiryOption{{Value: numberExpiryKeep, Label: "Leave unchanged"}},
		numberExpiryOptions()...,
	)
}

// numberBudgetMax is the cap the form draws in its input. Like the day count,
// it is a hint: the rule lives in elohim-veni, which owns the column.
const numberBudgetMax = 10000

// numberBudgetDefault is the sentinel that means "the owner did not choose, so
// let the authority apply its default".
//
// It is the EMPTY value and it is selected first, so a mint form submitted
// without a thought lands on the default rather than on no limit. That is the
// whole reason this control exists: a Number is now eleven payload digits, and
// an unlimited Number nobody chose is a harvested Number worth an unbounded
// amount. The default itself is elohim-veni's — it owns the column — and it is
// deliberately not restated here, where it would be a second answer waiting to
// disagree.
const numberBudgetDefault = ""

// numberBudgetUnlimited is the sentinel for an owner who wants no limit and says
// so. A creator putting a Number in their bio is the case, it is legitimate, and
// the default must not forbid it — only stop being what happens by accident.
const numberBudgetUnlimited = "unlimited"

// numberBudgetCustom is the sentinel that means "read max_admissions instead".
const numberBudgetCustom = "custom"

// numberBudgetOptions is the vocabulary a Number is minted with.
//
// The presets are the owner's own use cases in the sizes those rooms actually
// come in: a handful for a test group, a room for a class, a hall for a
// conference. "No limit" is last and never selected, because it is the choice
// that has to be made on purpose.
func numberBudgetOptions() []numberExpiryOption {
	return []numberExpiryOption{
		{Value: numberBudgetDefault, Label: "The usual limit — enough for a room, twice over"},
		{Value: "5", Label: "5 people — a project test group"},
		{Value: "12", Label: "12 people — a small class or a team"},
		{Value: "40", Label: "40 people — a class or a workshop"},
		{Value: "150", Label: "150 people — a conference track"},
		{Value: numberBudgetCustom, Label: "A number of people…"},
		{Value: numberBudgetUnlimited, Label: "No limit — anyone who has it, for ever"},
	}
}

// errBadLease is shown to the owner on their own surface. It never appears on a
// resolve, so it discloses nothing about anybody's Number.
var errBadLease = errors.New("that expiry or budget is not a whole number")

const secondsPerDay = 24 * 60 * 60

// numberLeaseInput is what the owner's form said about a lease.
//
// SAYING NOTHING AND SAYING "NONE" ARE DIFFERENT, and keeping them apart is the
// whole point of this type. The policy control beside a Number posts the policy
// and nothing else; if "no field" and "no limit" collapsed into one value, using
// that control would silently throw away the expiry and budget the owner set
// when they minted it. So an unmentioned field is omitted from the request
// entirely and elohim-veni leaves the column alone, while a field sent as null
// clears it.
type numberLeaseInput struct {
	ExpirySpecified  bool
	ExpiresInSeconds *int64
	BudgetSpecified  bool
	MaxAdmissions    *int
}

// applyTo adds only what the form actually said to the authority's request.
func (in numberLeaseInput) applyTo(body map[string]interface{}) {
	if in.ExpirySpecified {
		if in.ExpiresInSeconds == nil {
			body["expires_in_seconds"] = nil
		} else {
			body["expires_in_seconds"] = *in.ExpiresInSeconds
		}
	}
	if in.BudgetSpecified {
		if in.MaxAdmissions == nil {
			body["max_admissions"] = nil
		} else {
			body["max_admissions"] = *in.MaxAdmissions
		}
	}
}

// parseNumberLease reads the lease off the owner's form.
//
// It checks SHAPE only — is this a whole number, is it positive. The BOUNDS are
// elohim-veni's, because elohim-veni owns the columns; duplicating them here
// would put the real rule in two places and guarantee they eventually differ. A
// value this accepts and the authority refuses comes back as a refusal, which is
// the correct outcome for a value the authority does not allow.
func parseNumberLease(r *http.Request) (numberLeaseInput, error) {
	var in numberLeaseInput
	if err := r.ParseForm(); err != nil {
		return in, errBadLease
	}

	if r.Form.Has("expiry") {
		expiry := strings.TrimSpace(r.Form.Get("expiry"))
		if expiry == numberExpiryKeep {
			// The owner said nothing about the expiry. The field is dropped
			// from the request entirely, so the authority leaves the column
			// exactly as it is.
			return parseNumberBudget(r, in)
		}
		in.ExpirySpecified = true
		switch {
		case expiry == "":
			// The owner chose no expiry.
		case expiry == numberExpiryCustom:
			raw := strings.TrimSpace(r.Form.Get("expiry_days"))
			if raw == "" {
				// "A number of days" with no number is no expiry, not an error:
				// the owner opened the field and left it blank.
				break
			}
			days, convErr := strconv.ParseInt(raw, 10, 64)
			if convErr != nil || days <= 0 {
				return numberLeaseInput{}, errBadLease
			}
			secs := days * secondsPerDay
			in.ExpiresInSeconds = &secs
		default:
			secs, convErr := strconv.ParseInt(expiry, 10, 64)
			if convErr != nil || secs <= 0 {
				return numberLeaseInput{}, errBadLease
			}
			in.ExpiresInSeconds = &secs
		}
	}

	return parseNumberBudget(r, in)
}

// parseNumberBudget reads the budget half.
//
// TWO CONTROLS, ONE COLUMN, and the difference between them is deliberate.
//
// The MINT form posts a `budget` select. Its empty value means the owner did not
// choose, so nothing is sent and elohim-veni applies its default — which is the
// only way a default can exist at all, because a field that is always present
// leaves no room for one.
//
// The form that EDITS an existing Number posts `max_admissions` directly,
// pre-filled with the cap that Number already carries. There an empty field is
// an explicit "no limit", because the owner cleared a value that was there.
func parseNumberBudget(r *http.Request, in numberLeaseInput) (numberLeaseInput, error) {
	if r.Form.Has("budget") {
		switch budget := strings.TrimSpace(r.Form.Get("budget")); budget {
		case numberBudgetDefault:
			// Say nothing, so the authority's default applies.
			return in, nil
		case numberBudgetUnlimited:
			in.BudgetSpecified = true
			return in, nil
		case numberBudgetCustom:
			// Fall through to the number field beside the select.
		default:
			n, err := strconv.Atoi(budget)
			if err != nil || n <= 0 {
				return numberLeaseInput{}, errBadLease
			}
			in.BudgetSpecified = true
			in.MaxAdmissions = &n
			return in, nil
		}
		// "A number of people" with no number is the default, not no limit: the
		// owner opened the field and left it blank, and reading that as
		// "unlimited for ever" is the opposite of what they were reaching for.
		raw := strings.TrimSpace(r.Form.Get("max_admissions"))
		if raw == "" {
			return in, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return numberLeaseInput{}, errBadLease
		}
		in.BudgetSpecified = true
		in.MaxAdmissions = &n
		return in, nil
	}
	if !r.Form.Has("max_admissions") {
		return in, nil
	}
	in.BudgetSpecified = true
	raw := strings.TrimSpace(r.Form.Get("max_admissions"))
	if raw == "" {
		return in, nil
	}
	budget, err := strconv.Atoi(raw)
	if err != nil || budget <= 0 {
		return numberLeaseInput{}, errBadLease
	}
	in.MaxAdmissions = &budget
	return in, nil
}

// numberLease is one Number's lease as its OWNER sees it, already in words.
//
// The counts and the countdown are computed by the authority against its own
// clock and its own ledger; this struct only carries the answer. The browser is
// a projection surface and works none of it out.
type numberLease struct {
	// State is "live", "expired" or "exhausted". Only the owner ever sees it —
	// to a resolver all three of the last two, plus retired and unknown, are one
	// indistinguishable refusal.
	State string
	// Expires is the countdown in words, empty when the Number has no expiry.
	Expires string
	// Budget is the cap, Used is how many distinct identities this Number has
	// admitted, and Remaining is what is left. HasBudget is false for a Number
	// with no cap, where the other three mean nothing.
	HasBudget bool
	Budget    int
	Used      int
	Remaining int
}

// Live reports whether this Number is still admitting new contact.
func (l numberLease) Live() bool { return l.State == "" || l.State == "live" }

// Spent reports whether the budget is what stopped it, which is the one case
// the owner can undo without minting a new Number.
func (l numberLease) Spent() bool { return l.State == "exhausted" }

// Expired reports whether the lease is what stopped it.
func (l numberLease) Expired() bool { return l.State == "expired" }

// numberLeaseFrom turns the authority's answer into the owner's view.
//
// expiresIn is a count of seconds remaining, measured by elohim-veni against the
// clock that stamped the lease; a nil pointer means the Number has no expiry.
// budget is nil when the Number admits without limit.
func numberLeaseFrom(state string, expiresIn *int64, budget *int, used int) numberLease {
	l := numberLease{State: state, Used: used}
	if expiresIn != nil {
		l.Expires = humaniseTimeLeft(*expiresIn)
	}
	if budget != nil {
		l.HasBudget = true
		l.Budget = *budget
		l.Remaining = *budget - used
		if l.Remaining < 0 {
			l.Remaining = 0
		}
	}
	return l
}

// humaniseTimeLeft renders a countdown the way a person would say it.
//
// Rendered on the server, like everything else: a countdown computed in the
// browser would be a second clock disagreeing with the one that enforces the
// lease, and the browser decides nothing.
func humaniseTimeLeft(seconds int64) string {
	switch {
	case seconds <= 0:
		return "expired"
	case seconds < 60:
		return "under a minute left"
	case seconds < 3600:
		return plural(seconds/60, "minute") + " left"
	case seconds < secondsPerDay:
		return plural(seconds/3600, "hour") + " left"
	default:
		return plural(seconds/secondsPerDay, "day") + " left"
	}
}

func plural(n int64, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
