//! Axiomatic Knowledge Graph queries over a state snapshot.
//!
//! All functions here are pure reads of `State`. They compute whether a
//! record is usable for a decision; they never grant a capability.

use std::collections::{BTreeMap, BTreeSet, VecDeque};

use serde::Serialize;

use crate::canonical::digest;
use crate::deduce::{Problem, Rule};
use crate::error::Result;
use crate::policy::Sensitivity;
use crate::state::{Basis, DerivationStatus, Lifecycle, Literal, Node, NodeBody, State};

/// Conservative whole-project dependency fingerprint.
///
/// Covers everything a decision may depend on — approved root, policy,
/// every knowledge record's exact revision and lifecycle, protected test
/// manifests and environments — but deliberately NOT audit-only history
/// (proposals, verification receipts, assessments, evaluations), so recording
/// a check does not invalidate the check.
pub fn dependency_fingerprint(s: &State) -> Result<String> {
    #[derive(Serialize)]
    struct Fp<'a> {
        approved_root: &'a str,
        policy_digest: &'a str,
        knowledge: BTreeMap<&'a str, Lifecycle>,
        test_manifests: BTreeMap<&'a str, &'a str>,
        environments: BTreeMap<&'a str, &'a str>,
    }
    digest(
        "fingerprint",
        &Fp {
            approved_root: &s.approved_root,
            policy_digest: &s.policy_digest,
            knowledge: s
                .nodes
                .iter()
                .map(|(k, n)| (k.as_str(), n.lifecycle))
                .collect(),
            test_manifests: s
                .test_manifests
                .iter()
                .map(|(k, m)| (k.as_str(), m.digest.as_str()))
                .collect(),
            environments: s
                .environments
                .iter()
                .map(|(k, e)| (k.as_str(), e.digest.as_str()))
                .collect(),
        },
    )
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Epistemic {
    Observed,
    Assumed,
    DerivedUnderPremises,
    Unsupported,
    Disputed,
}

fn current<'a>(s: &'a State, r: &str) -> Option<&'a Node> {
    s.nodes.get(r).filter(|n| n.lifecycle == Lifecycle::Current)
}

fn assumption_live(n: &Node, now: u64) -> bool {
    match &n.body {
        NodeBody::Assumption { expires_at, .. } => {
            n.lifecycle == Lifecycle::Current && expires_at.is_none_or(|t| now < t)
        }
        _ => false,
    }
}

/// Direct (non-derivation) support for an observed/assumed claim.
fn direct_support_ok(s: &State, basis: Basis, support: &[String], now: u64) -> bool {
    if support.is_empty() {
        return false;
    }
    support.iter().all(|r| match (basis, s.nodes.get(r)) {
        (Basis::Observed, Some(n)) => {
            n.lifecycle == Lifecycle::Current && matches!(n.body, NodeBody::Evidence { .. })
        }
        (Basis::Assumed, Some(n)) => assumption_live(n, now),
        _ => false,
    })
}

fn has_valid_derivation(s: &State, claim_ref: &str) -> bool {
    s.nodes.values().any(|n| match &n.body {
        NodeBody::Derivation { conclusion, .. } => {
            conclusion == claim_ref
                && n.lifecycle == Lifecycle::Current
                && s.derivation_status.get(&n.node_ref).map(|e| e.status)
                    == Some(DerivationStatus::Valid)
        }
        _ => false,
    })
}

/// Supported ignoring contradictions.
fn supported(s: &State, n: &Node, now: u64) -> Option<Epistemic> {
    if n.lifecycle != Lifecycle::Current {
        return None;
    }
    let NodeBody::Claim {
        basis,
        support_refs,
        ..
    } = &n.body
    else {
        return None;
    };
    match basis {
        Basis::Observed if direct_support_ok(s, *basis, support_refs, now) => {
            Some(Epistemic::Observed)
        }
        Basis::Assumed if direct_support_ok(s, *basis, support_refs, now) => {
            Some(Epistemic::Assumed)
        }
        _ if has_valid_derivation(s, &n.node_ref) => Some(Epistemic::DerivedUnderPremises),
        _ => None,
    }
}

/// Epistemic status of a claim for a decision made now.
pub fn epistemic(s: &State, claim_ref: &str, now: u64) -> Epistemic {
    let Some(n) = s.nodes.get(claim_ref) else {
        return Epistemic::Unsupported;
    };
    let Some(status) = supported(s, n, now) else {
        return Epistemic::Unsupported;
    };
    if let NodeBody::Claim {
        literal: Some(lit),
        context,
        ..
    } = &n.body
    {
        // Material contrary support in the same context makes the claim disputed.
        let contrary = s.nodes.values().any(|m| match &m.body {
            NodeBody::Claim {
                literal: Some(l2),
                context: c2,
                ..
            } => {
                c2 == context
                    && l2.atom == lit.atom
                    && l2.positive != lit.positive
                    && supported(s, m, now).is_some()
            }
            NodeBody::Assumption {
                literal: Some(l2),
                context: c2,
                ..
            } => {
                c2 == context
                    && l2.atom == lit.atom
                    && l2.positive != lit.positive
                    && assumption_live(m, now)
            }
            _ => false,
        });
        let flagged = s
            .contexts
            .get(context)
            .is_some_and(|c| c.conflicts.contains(&lit.atom));
        if contrary || flagged {
            return Epistemic::Disputed;
        }
    }
    status
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PremiseProblem {
    Unknown,
    NotCurrent(Lifecycle),
    Unsupported,
    Disputed,
    Expired,
}

impl PremiseProblem {
    pub fn code(&self) -> &'static str {
        match self {
            PremiseProblem::Unknown => "UNKNOWN_PREMISE",
            PremiseProblem::NotCurrent(_) => "STALE_PREMISE",
            PremiseProblem::Unsupported => "UNSUPPORTED_PREMISE",
            PremiseProblem::Disputed => "DISPUTED_PREMISE",
            PremiseProblem::Expired => "EXPIRED_PREMISE",
        }
    }
}

/// May this exact record revision support a decision now?
pub fn premise_usable(s: &State, r: &str, now: u64) -> std::result::Result<(), PremiseProblem> {
    let n = s.nodes.get(r).ok_or(PremiseProblem::Unknown)?;
    if n.lifecycle != Lifecycle::Current {
        return Err(PremiseProblem::NotCurrent(n.lifecycle));
    }
    match &n.body {
        NodeBody::Requirement { .. } | NodeBody::Evidence { .. } | NodeBody::Rule { .. } => Ok(()),
        NodeBody::Assumption { .. } => {
            if assumption_live(n, now) {
                Ok(())
            } else {
                Err(PremiseProblem::Expired)
            }
        }
        NodeBody::Claim { .. } => match epistemic(s, r, now) {
            Epistemic::Unsupported => Err(PremiseProblem::Unsupported),
            Epistemic::Disputed => Err(PremiseProblem::Disputed),
            _ => Ok(()),
        },
        NodeBody::Derivation { .. } => {
            if s.derivation_status.get(r).map(|e| e.status) == Some(DerivationStatus::Valid) {
                Ok(())
            } else {
                Err(PremiseProblem::Unsupported)
            }
        }
    }
}

/// Invalidation boundary of revoking `target`: derivations made stale and
/// observed/assumed claims whose direct support is lost. Conservative: the
/// traversal continues through every dependent.
pub fn invalidation_closure(s: &State, target: &str) -> (Vec<String>, Vec<String>) {
    let mut invalid: BTreeSet<String> = BTreeSet::from([target.to_string()]);
    let mut derivations = BTreeSet::new();
    let mut claims = BTreeSet::new();
    let mut queue = VecDeque::from([target.to_string()]);
    let mut seen = BTreeSet::from([target.to_string()]);
    while let Some(cur) = queue.pop_front() {
        let Some(deps) = s.dependents.get(&cur) else {
            continue;
        };
        for d in deps {
            let Some(n) = s.nodes.get(d) else { continue };
            match &n.body {
                NodeBody::Derivation { .. } => {
                    derivations.insert(d.clone());
                    invalid.insert(d.clone());
                }
                NodeBody::Claim {
                    basis,
                    support_refs,
                    ..
                } => {
                    if matches!(basis, Basis::Observed | Basis::Assumed)
                        && support_refs.iter().any(|x| invalid.contains(x))
                    {
                        claims.insert(d.clone());
                        invalid.insert(d.clone());
                    }
                }
                _ => {}
            }
            if seen.insert(d.clone()) {
                queue.push_back(d.clone());
            }
        }
    }
    (
        derivations.into_iter().collect(),
        claims.into_iter().collect(),
    )
}

/// A formal context compiled to the deductor's integer problem.
pub struct CompiledContext {
    pub problem: Problem,
    pub atoms: Vec<String>,
    /// Horn rule index -> derivation ref.
    pub rule_derivations: Vec<String>,
    /// Derivation ref -> premise literal ints (for validity after solving).
    pub derivation_bodies: BTreeMap<String, Vec<i64>>,
    /// Derivations rejected structurally before solving, with reason.
    pub invalid: BTreeMap<String, String>,
    /// Exact premise versions this evaluation depends on.
    pub premise_refs: Vec<String>,
}

fn lit_int(atoms: &BTreeMap<String, usize>, l: &Literal) -> i64 {
    let i = atoms[&l.atom] as i64;
    if l.positive {
        i
    } else {
        -i
    }
}

fn claim_literal<'a>(s: &'a State, r: &str) -> Option<&'a Literal> {
    match s.nodes.get(r).map(|n| &n.body) {
        Some(NodeBody::Claim {
            literal: Some(l), ..
        }) => Some(l),
        _ => None,
    }
}

pub fn compile_context(s: &State, context_id: &str, now: u64) -> CompiledContext {
    let mut facts: Vec<(String, Literal)> = Vec::new();
    let mut derivs: Vec<(String, Vec<Literal>, Literal, String)> = Vec::new(); // (ref, body, head, rule ref)
    let mut invalid = BTreeMap::new();

    for n in s.nodes.values() {
        match &n.body {
            NodeBody::Claim {
                context,
                literal: Some(lit),
                basis,
                support_refs,
                ..
            } if context == context_id => {
                if n.lifecycle == Lifecycle::Current
                    && matches!(basis, Basis::Observed | Basis::Assumed)
                    && direct_support_ok(s, *basis, support_refs, now)
                {
                    facts.push((n.node_ref.clone(), lit.clone()));
                }
            }
            NodeBody::Assumption {
                context,
                literal: Some(lit),
                ..
            } if context == context_id => {
                if assumption_live(n, now) {
                    facts.push((n.node_ref.clone(), lit.clone()));
                }
            }
            NodeBody::Derivation {
                context,
                conclusion,
                premises,
                rule,
            } if context == context_id => {
                if n.lifecycle != Lifecycle::Current {
                    continue;
                }
                let rule_node = current(s, rule);
                let Some(NodeBody::Rule { body, head }) = rule_node.map(|r| &r.body) else {
                    invalid.insert(n.node_ref.clone(), "RULE_NOT_CURRENT".into());
                    continue;
                };
                let concl: Option<&Literal> =
                    current(s, conclusion).and_then(|_| claim_literal(s, conclusion));
                let prem: Option<Vec<Literal>> = premises
                    .iter()
                    .map(|p| current(s, p).and_then(|_| claim_literal(s, p)).cloned())
                    .collect();
                let (Some(concl), Some(prem)) = (concl, prem) else {
                    invalid.insert(
                        n.node_ref.clone(),
                        "PREMISE_OR_CONCLUSION_NOT_CURRENT_FORMAL_CLAIM".into(),
                    );
                    continue;
                };
                let a: BTreeSet<&Literal> = prem.iter().collect();
                let b: BTreeSet<&Literal> = body.iter().collect();
                if a != b || concl != head {
                    invalid.insert(n.node_ref.clone(), "RULE_DOES_NOT_ENTAIL_CONCLUSION".into());
                    continue;
                }
                if premises.iter().any(|p| p == conclusion) {
                    invalid.insert(n.node_ref.clone(), "CIRCULAR_SELF_SUPPORT".into());
                    continue;
                }
                derivs.push((n.node_ref.clone(), prem, concl.clone(), rule.clone()));
            }
            _ => {}
        }
    }

    let mut names: BTreeSet<String> = BTreeSet::new();
    for (_, l) in &facts {
        names.insert(l.atom.clone());
    }
    for (_, body, head, _) in &derivs {
        names.insert(head.atom.clone());
        for l in body {
            names.insert(l.atom.clone());
        }
    }
    let atoms: Vec<String> = names.into_iter().collect();
    let index: BTreeMap<String, usize> = atoms
        .iter()
        .enumerate()
        .map(|(i, a)| (a.clone(), i + 1))
        .collect();

    let mut premise_refs: BTreeSet<String> = BTreeSet::new();
    let problem_facts: Vec<i64> = facts
        .iter()
        .map(|(r, l)| {
            premise_refs.insert(r.clone());
            lit_int(&index, l)
        })
        .collect();
    let mut rules = Vec::new();
    let mut rule_derivations = Vec::new();
    let mut derivation_bodies = BTreeMap::new();
    for (r, body, head, rule_ref) in &derivs {
        premise_refs.insert(r.clone());
        premise_refs.insert(rule_ref.clone());
        let b: Vec<i64> = body.iter().map(|l| lit_int(&index, l)).collect();
        derivation_bodies.insert(r.clone(), b.clone());
        rules.push(Rule {
            head: lit_int(&index, head),
            body: b,
        });
        rule_derivations.push(r.clone());
    }
    CompiledContext {
        problem: Problem {
            atoms: atoms.len(),
            facts: problem_facts,
            rules,
        },
        atoms,
        rule_derivations,
        derivation_bodies,
        invalid,
        premise_refs: premise_refs.into_iter().collect(),
    }
}

/// Effective sensitivity: a record inherits the highest sensitivity of the
/// records it is built from (a summary of restricted input is restricted),
/// so its body can never reveal a record the caller may not see.
pub fn effective_sensitivity(s: &State, n: &Node) -> Sensitivity {
    let mut level = n.meta.sensitivity;
    let mut deps = n.body.dependencies();
    if let NodeBody::Derivation { conclusion, .. } = &n.body {
        deps.push(conclusion.clone());
    }
    for d in deps {
        if level == Sensitivity::Restricted {
            break;
        }
        // Every dependency (and a derivation's conclusion) must exist before
        // the dependent is added, so the recursion is well-founded.
        if let Some(m) = s.nodes.get(&d) {
            level = level.max(effective_sensitivity(s, m));
        }
    }
    level
}

pub fn visible(s: &State, n: &Node, clearance: Sensitivity) -> bool {
    effective_sensitivity(s, n) <= clearance
}

/// Permission-filtered dependency expansion. Restricted nodes are neither
/// returned nor traversed, so paths through them leak nothing. A start node
/// the caller cannot see is indistinguishable from a missing one.
pub fn expand<'a>(
    s: &'a State,
    start: &str,
    clearance: Sensitivity,
    depth: usize,
) -> Option<Vec<&'a Node>> {
    let root = s.nodes.get(start).filter(|n| visible(s, n, clearance))?;
    let mut out = vec![root];
    let mut seen = BTreeSet::from([start.to_string()]);
    let mut frontier = vec![start.to_string()];
    for _ in 0..depth {
        let mut next = Vec::new();
        for r in &frontier {
            let mut neigh: Vec<String> = s
                .nodes
                .get(r)
                .map(|n| n.body.dependencies())
                .unwrap_or_default();
            if let Some(NodeBody::Derivation { conclusion, .. }) = s.nodes.get(r).map(|n| &n.body) {
                neigh.push(conclusion.clone());
            }
            neigh.extend(s.dependents.get(r).into_iter().flatten().cloned());
            for x in neigh {
                if let Some(n) = s.nodes.get(&x) {
                    if visible(s, n, clearance) && seen.insert(x.clone()) {
                        out.push(n);
                        next.push(x);
                    }
                }
            }
        }
        frontier = next;
    }
    Some(out)
}
