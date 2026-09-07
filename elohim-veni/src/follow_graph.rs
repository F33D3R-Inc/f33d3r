//! The follow graph, read rather than believed.
//!
//! Two contact policies — `followers` and `mutuals` — are decisions about who
//! follows whom. The follow graph is feed-engine's: it lives in `f33d3r_feed`,
//! it is traversed by ranking and suggestion, and it is not moving here.
//!
//! What changed is who answers the question. The contact-evaluate call used to
//! carry `"follows": true` and `"mutual": true` as plain booleans supplied by
//! feed-engine, and this brain believed them. That is a security decision taken
//! on an assertion the deciding brain cannot verify, and it is exactly the
//! coupling Manhattan exists to eliminate.
//!
//! It also did not work. On the Number path this brain will not disclose who a
//! Number belongs to until it has decided — that is the whole promise of a
//! Number — so feed-engine had nobody to compute the facts about, sent none, and
//! `followers` and `mutuals` silently degraded to "open a contact request" for
//! people who genuinely were mutual follows. The privacy property and the
//! feature were in direct conflict and the feature lost.
//!
//! Reading the graph here settles both. This brain already knows the target: it
//! resolved the Number itself. feed-engine still never learns who the Number
//! belongs to, because it is never asked. And the fact this brain acts on is one
//! it fetched, from the plane whose entire purpose is that no brain has to take
//! another brain's word for anything.

use crate::contact::{self, ContactFacts};
use manhattan_client::{AssocBetween, Manhattan, ManhattanError, ASSOC_FOLLOWS};
use uuid::Uuid;

/// What the graph says about one ordered pair.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct FollowFacts {
    /// The initiator follows the owner.
    pub follows: bool,
    /// Each follows the other. On F33D3R a contact IS a mutual follow.
    pub mutual: bool,
}

/// Whether a policy's outcome can turn on the follow graph at all.
///
/// Derived from `decide` rather than listed, for the same reason
/// `number_was_load_bearing` re-runs `decide` instead of enumerating the
/// policies it applies to: a list here would be a second copy of the policy
/// vocabulary, and the copy is what drifts. A policy added tomorrow that reads
/// the graph is handled by this function the day it is added — and, just as
/// importantly, a policy that does NOT read the graph never costs a round trip
/// to the naming plane.
pub fn depends_on_follow_graph(policy: &str) -> bool {
    // `mutual` implies `follows`, so those are the only reachable combinations.
    const COMBINATIONS: [(bool, bool); 3] = [(false, false), (true, false), (true, true)];
    let outcome = |follows: bool, mutual: bool, via_number: bool| {
        contact::decide(
            policy,
            &ContactFacts {
                via_number,
                follows,
                mutual,
                granted: false,
                capability_ok: false,
            },
        )
    };
    for via_number in [false, true] {
        let baseline = outcome(false, false, via_number);
        if COMBINATIONS
            .iter()
            .any(|&(f, m)| outcome(f, m, via_number) != baseline)
        {
            return true;
        }
    }
    false
}

/// Reads both directions between two identities in one round trip.
///
/// A name that does not resolve yields `follows: false`, and that is a
/// statement rather than a fallback: an association exists only between two
/// resolved nodes, so a name the plane has never seen can carry none. It is
/// still reported by the caller, because an identity missing from the plane is
/// an undrained outbox and somebody should know.
///
/// A transport failure is NOT converted into `false`. "Not following" and "could
/// not ask" are different, and only one of them may quietly refuse contact; the
/// other has to be loud, so it comes back as an error and the caller fails
/// closed on it.
pub async fn between(
    manhattan: &Manhattan,
    initiator: Uuid,
    owner: Uuid,
) -> Result<(FollowFacts, AssocBetween), ManhattanError> {
    let subject = manhattan_client::pial_name(&initiator.to_string());
    let object = manhattan_client::pial_name(&owner.to_string());
    let answer = manhattan
        .assoc_between(ASSOC_FOLLOWS, &subject, &object)
        .await?;
    Ok((
        FollowFacts {
            follows: answer.forward,
            mutual: answer.mutual,
        },
        answer,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The two relationship policies must ask the graph, and nothing else may —
    /// a policy that cannot change its answer on a follow must not cost a call
    /// to the naming plane on the hot path of a contact decision.
    #[test]
    fn only_the_relationship_policies_read_the_graph() {
        assert!(depends_on_follow_graph("followers"));
        assert!(depends_on_follow_graph("mutuals"));
        for p in ["open", "number_only", "capability_only", "closed"] {
            assert!(!depends_on_follow_graph(p), "policy {p} asked the graph");
        }
    }

    /// An unknown policy denies whatever the graph says, so it must not spend a
    /// round trip finding out.
    #[test]
    fn an_unknown_policy_does_not_ask() {
        assert!(!depends_on_follow_graph("whatever"));
    }

    /// This is the derivation, not a list: every policy in the vocabulary is
    /// classified by running the real decision function, so the classification
    /// cannot fall out of step with the rule it describes.
    #[test]
    fn the_classification_is_derived_from_the_decision_itself() {
        for policy in contact::POLICIES {
            let base = ContactFacts {
                via_number: true,
                follows: false,
                mutual: false,
                granted: false,
                capability_ok: false,
            };
            let related = ContactFacts {
                follows: true,
                mutual: true,
                ..base
            };
            let differs = contact::decide(policy, &base) != contact::decide(policy, &related);
            assert_eq!(
                differs,
                depends_on_follow_graph(policy),
                "policy {policy} is classified against what decide() actually does"
            );
        }
    }

    /// Mutuality is Manhattan's answer, not one this brain recomputes from two
    /// halves it fetched separately — one round trip, one consistent snapshot.
    #[test]
    fn facts_come_straight_from_the_plane() {
        let answer = AssocBetween {
            subject_resolved: true,
            object_resolved: true,
            forward: true,
            reverse: false,
            mutual: false,
        };
        let facts = FollowFacts {
            follows: answer.forward,
            mutual: answer.mutual,
        };
        assert!(facts.follows);
        assert!(!facts.mutual);
        assert_eq!(contact::decide("followers", &facts_with(facts)).0, "allow");
        assert_eq!(contact::decide("mutuals", &facts_with(facts)).0, "request");
    }

    fn facts_with(f: FollowFacts) -> ContactFacts {
        ContactFacts {
            via_number: true,
            follows: f.follows,
            mutual: f.mutual,
            granted: false,
            capability_ok: false,
        }
    }
}
