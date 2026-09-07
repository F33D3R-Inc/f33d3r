//! The Frequency state machine.
//!
//! One table of permitted transitions. Every mutation that changes `state`
//! goes through [`transition`] before it is written, and the compare-and-set
//! on `version` makes sure the state it checked is the state it changed.
//!
//! ```text
//! draft ──► scheduled ──► starting ──► live ──► ending ──► ended ──► processing_replay ──► archived
//!   │           │            │          │                      │                            ▲
//!   │           │            │          ├──► moderation_terminated                          │
//!   │           │            ├──► failed│                                                   │
//!   ├──► cancelled ◄─────────┘          └──► failed        ended ─────────────────────────────┘
//! ```
//!
//! Never: ended → live, cancelled → live, archived → starting. A new session
//! is a new Frequency.

use super::frequency::FrequencyState;

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("cannot move a frequency from {from} to {to}")]
pub struct InvalidTransition {
    pub from: FrequencyState,
    pub to: FrequencyState,
}

/// The permitted moves. Exhaustive on purpose: adding a state without adding
/// its rows here leaves it unreachable, which is the safe default.
pub fn allowed(from: FrequencyState, to: FrequencyState) -> bool {
    use FrequencyState::*;
    matches!(
        (from, to),
        (Draft, Scheduled)
            | (Draft, Starting)
            | (Draft, Cancelled)
            | (Scheduled, Draft)
            | (Scheduled, Starting)
            | (Scheduled, Cancelled)
            | (Starting, Live)
            | (Starting, Failed)
            | (Starting, ModerationTerminated)
            | (Live, Ending)
            | (Live, Failed)
            | (Live, ModerationTerminated)
            | (Ending, Ended)
            | (Ending, Failed)
            | (Ended, ProcessingReplay)
            | (Ended, Archived)
            | (ProcessingReplay, Archived)
    )
}

pub fn transition(from: FrequencyState, to: FrequencyState) -> Result<(), InvalidTransition> {
    if allowed(from, to) {
        Ok(())
    } else {
        Err(InvalidTransition { from, to })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use FrequencyState::*;

    #[test]
    fn the_happy_path_is_allowed_in_order() {
        let path = [
            Draft,
            Scheduled,
            Starting,
            Live,
            Ending,
            Ended,
            ProcessingReplay,
            Archived,
        ];
        for w in path.windows(2) {
            assert!(transition(w[0], w[1]).is_ok(), "{} -> {}", w[0], w[1]);
        }
    }

    #[test]
    fn an_unscheduled_draft_can_start_directly() {
        assert!(transition(Draft, Starting).is_ok());
    }

    /// The directive's three named prohibitions, plus every other way back
    /// into a session from a state that is over.
    #[test]
    fn nothing_over_ever_goes_live_again() {
        for from in FrequencyState::ALL.into_iter().filter(|s| s.is_over()) {
            for to in [Starting, Live, Scheduled, Draft] {
                assert_eq!(
                    transition(from, to),
                    Err(InvalidTransition { from, to }),
                    "{from} -> {to} must be refused"
                );
            }
        }
    }

    #[test]
    fn terminal_states_have_no_exit() {
        for from in FrequencyState::ALL.into_iter().filter(|s| s.is_terminal()) {
            for to in FrequencyState::ALL {
                assert!(!allowed(from, to), "{from} -> {to}");
            }
        }
    }

    #[test]
    fn moderation_can_terminate_only_a_session() {
        assert!(allowed(Starting, ModerationTerminated));
        assert!(allowed(Live, ModerationTerminated));
        assert!(!allowed(Ended, ModerationTerminated));
        assert!(!allowed(Scheduled, ModerationTerminated));
    }

    #[test]
    fn a_state_never_transitions_to_itself() {
        for s in FrequencyState::ALL {
            assert!(!allowed(s, s), "{s} -> {s}");
        }
    }

    #[test]
    fn ending_only_leads_to_ended_or_failed() {
        for to in FrequencyState::ALL {
            assert_eq!(allowed(Ending, to), matches!(to, Ended | Failed), "{to}");
        }
    }
}
