//! Bounded Horn-fragment deduction (PROTOCOL.md §7).
//!
//! This is the trusted Rust reference implementation. The Mojo deductor is
//! an accelerated implementation of the same semantics; in `Cross` mode both
//! run and any disagreement fails closed.

use std::fmt::Write as _;
use std::path::PathBuf;
use std::process::Command;

use crate::canonical::blob_digest;
use crate::error::{CoreError, Result};

pub const MAX_ATOMS: usize = 1_000_000;
pub const MAX_RULES: usize = 1_000_000;
pub const MAX_BODY_TOTAL: usize = 10_000_000;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rule {
    pub head: i64,
    pub body: Vec<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Problem {
    pub atoms: usize,
    pub facts: Vec<i64>,
    pub rules: Vec<Rule>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DerivedLiteral {
    pub literal: i64,
    /// Index of the justifying rule, or -1 for a fact.
    pub justification: i64,
    pub depth: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Deduced {
    pub derived: Vec<DerivedLiteral>,
    pub conflicts: Vec<u64>,
    pub consistent: bool,
}

impl Deduced {
    pub fn contains(&self, lit: i64) -> bool {
        self.derived.iter().any(|d| d.literal == lit)
    }
}

fn malformed(msg: impl Into<String>) -> CoreError {
    CoreError::new("DEDUCE_MALFORMED", msg)
}

fn check_lit(lit: i64, atoms: usize) -> Result<i64> {
    if lit == 0 || lit.unsigned_abs() as usize > atoms {
        return Err(malformed(format!("literal {lit} out of range")));
    }
    Ok(lit)
}

struct Lines<'a> {
    iter: std::iter::Peekable<std::str::Split<'a, char>>,
}

impl<'a> Lines<'a> {
    fn new(text: &'a str) -> Result<Self> {
        let body = text.strip_suffix('\n').unwrap_or(text);
        if body.contains('\r') {
            return Err(malformed("carriage return"));
        }
        Ok(Lines {
            iter: body.split('\n').peekable(),
        })
    }
    fn next(&mut self) -> Result<&'a str> {
        self.iter
            .next()
            .ok_or_else(|| malformed("unexpected end of input"))
    }
    fn tokens(&mut self) -> Result<Vec<&'a str>> {
        let line = self.next()?;
        let toks: Vec<&str> = line.split(' ').collect();
        if toks.iter().any(|t| t.is_empty()) {
            return Err(malformed(format!("bad spacing in line {line:?}")));
        }
        Ok(toks)
    }
    fn keyword_count(&mut self, kw: &str, limit: usize) -> Result<usize> {
        let t = self.tokens()?;
        if t.len() != 2 || t[0] != kw {
            return Err(malformed(format!("expected '{kw} <n>'")));
        }
        let n = parse_uint(t[1])?;
        if n > limit {
            return Err(malformed(format!("{kw} exceeds limit")));
        }
        Ok(n)
    }
    fn expect(&mut self, s: &str) -> Result<()> {
        if self.next()? != s {
            return Err(malformed(format!("expected {s:?}")));
        }
        Ok(())
    }
    fn finish(&mut self) -> Result<()> {
        if self.iter.next().is_some() {
            return Err(malformed("trailing content after end"));
        }
        Ok(())
    }
}

/// Strict grammar shared with the Mojo deductor: `0 | [1-9][0-9]*`.
fn canonical_digits(t: &str) -> bool {
    !t.is_empty() && t.bytes().all(|b| b.is_ascii_digit()) && (t == "0" || !t.starts_with('0'))
}

fn parse_uint(t: &str) -> Result<usize> {
    if !canonical_digits(t) {
        return Err(malformed(format!("bad count {t:?}")));
    }
    t.parse::<usize>()
        .map_err(|_| malformed(format!("bad count {t:?}")))
}

/// Optional leading `-` only; no `+`, no leading zeros, no `-0`.
fn parse_int(t: &str) -> Result<i64> {
    let digits = t.strip_prefix('-').unwrap_or(t);
    if !canonical_digits(digits) || t == "-0" {
        return Err(malformed(format!("bad integer {t:?}")));
    }
    t.parse::<i64>()
        .map_err(|_| malformed(format!("bad integer {t:?}")))
}

impl Problem {
    pub fn parse(text: &str) -> Result<Problem> {
        let mut l = Lines::new(text)?;
        l.expect("INTELLECTUS-DEDUCE 1")?;
        let atoms = l.keyword_count("atoms", MAX_ATOMS)?;
        let nf = l.keyword_count("facts", usize::MAX)?;
        let mut facts = Vec::with_capacity(nf.min(1 << 20));
        for _ in 0..nf {
            let t = l.tokens()?;
            if t.len() != 1 {
                return Err(malformed("fact line must hold one literal"));
            }
            facts.push(check_lit(parse_int(t[0])?, atoms)?);
        }
        let nr = l.keyword_count("rules", MAX_RULES)?;
        let mut rules = Vec::with_capacity(nr.min(1 << 20));
        let mut total = 0usize;
        for _ in 0..nr {
            let t = l.tokens()?;
            if t.len() < 2 {
                return Err(malformed("rule line too short"));
            }
            let head = check_lit(parse_int(t[0])?, atoms)?;
            let n = parse_uint(t[1])?;
            if t.len() != n + 2 {
                return Err(malformed("rule body count mismatch"));
            }
            total += n;
            if total > MAX_BODY_TOTAL {
                return Err(malformed("total body literals exceed limit"));
            }
            let body = t[2..]
                .iter()
                .map(|x| parse_int(x).and_then(|v| check_lit(v, atoms)))
                .collect::<Result<Vec<_>>>()?;
            rules.push(Rule { head, body });
        }
        l.expect("end")?;
        l.finish()?;
        Ok(Problem {
            atoms,
            facts,
            rules,
        })
    }

    pub fn render(&self) -> String {
        let mut s = String::new();
        let _ = writeln!(
            s,
            "INTELLECTUS-DEDUCE 1\natoms {}\nfacts {}",
            self.atoms,
            self.facts.len()
        );
        for f in &self.facts {
            let _ = writeln!(s, "{f}");
        }
        let _ = writeln!(s, "rules {}", self.rules.len());
        for r in &self.rules {
            let _ = write!(s, "{} {}", r.head, r.body.len());
            for b in &r.body {
                let _ = write!(s, " {b}");
            }
            s.push('\n');
        }
        s.push_str("end\n");
        s
    }
}

#[inline]
fn slot(lit: i64) -> usize {
    ((lit.unsigned_abs() as usize - 1) << 1) | usize::from(lit < 0)
}

#[inline]
fn lit_of(slot: usize) -> i64 {
    let atom = (slot >> 1) as i64 + 1;
    if slot & 1 == 1 {
        -atom
    } else {
        atom
    }
}

/// Jacobi-round least fixpoint with deterministic justifications.
pub fn solve(p: &Problem) -> Deduced {
    let nslots = p.atoms * 2;
    let mut just: Vec<i64> = vec![i64::MIN; nslots]; // MIN = not derived
    let mut depth: Vec<u64> = vec![0; nslots];

    // Distinct body literals per rule and the watch lists over them.
    let mut remaining: Vec<usize> = Vec::with_capacity(p.rules.len());
    let mut watchers: Vec<Vec<u32>> = vec![Vec::new(); nslots];
    for (ri, r) in p.rules.iter().enumerate() {
        let mut body: Vec<usize> = r.body.iter().map(|&b| slot(b)).collect();
        body.sort_unstable();
        body.dedup();
        remaining.push(body.len());
        for s in body {
            watchers[s].push(ri as u32);
        }
    }

    let mut candidates: Vec<u32> = (0..p.rules.len() as u32)
        .filter(|&r| remaining[r as usize] == 0)
        .collect();
    let mut frontier: Vec<usize> = Vec::new();
    for &f in &p.facts {
        let s = slot(f);
        if just[s] == i64::MIN {
            just[s] = -1;
            frontier.push(s);
        }
    }

    let mut round: u64 = 0;
    loop {
        // Rules completed by literals added in `round` become candidates for round+1.
        for &s in &frontier {
            for &r in &watchers[s] {
                let rem = &mut remaining[r as usize];
                *rem -= 1;
                if *rem == 0 {
                    candidates.push(r);
                }
            }
        }
        frontier.clear();
        if candidates.is_empty() {
            break;
        }
        round += 1;
        // Choose the smallest rule index per new head among this round's candidates.
        let mut best: Vec<(usize, u32)> = Vec::new();
        for &r in &candidates {
            let h = slot(p.rules[r as usize].head);
            if just[h] == i64::MIN {
                best.push((h, r));
            }
        }
        candidates.clear();
        best.sort_unstable();
        let mut i = 0;
        while i < best.len() {
            let (h, r) = best[i];
            just[h] = r as i64;
            depth[h] = round;
            frontier.push(h);
            while i < best.len() && best[i].0 == h {
                i += 1;
            }
        }
        if frontier.is_empty() {
            break;
        }
    }

    let mut derived = Vec::new();
    let mut conflicts = Vec::new();
    for atom in 0..p.atoms {
        let (pos, neg) = (atom << 1, (atom << 1) | 1);
        for s in [pos, neg] {
            if just[s] != i64::MIN {
                derived.push(DerivedLiteral {
                    literal: lit_of(s),
                    justification: just[s],
                    depth: depth[s],
                });
            }
        }
        if just[pos] != i64::MIN && just[neg] != i64::MIN {
            conflicts.push(atom as u64 + 1);
        }
    }
    let consistent = conflicts.is_empty();
    Deduced {
        derived,
        conflicts,
        consistent,
    }
}

impl Deduced {
    pub fn render(&self) -> String {
        let mut s = String::new();
        let _ = writeln!(s, "INTELLECTUS-DEDUCED 1\nderived {}", self.derived.len());
        for d in &self.derived {
            let _ = writeln!(s, "{} {} {}", d.literal, d.justification, d.depth);
        }
        let _ = writeln!(s, "conflicts {}", self.conflicts.len());
        for c in &self.conflicts {
            let _ = writeln!(s, "{c}");
        }
        let _ = writeln!(s, "consistent {}", u8::from(self.consistent));
        s.push_str("end\n");
        s
    }

    pub fn parse(text: &str) -> Result<Deduced> {
        let mut l = Lines::new(text)?;
        l.expect("INTELLECTUS-DEDUCED 1")?;
        let n = l.keyword_count("derived", usize::MAX)?;
        let mut derived = Vec::with_capacity(n.min(1 << 20));
        for _ in 0..n {
            let t = l.tokens()?;
            if t.len() != 3 {
                return Err(malformed("derived line must have 3 fields"));
            }
            derived.push(DerivedLiteral {
                literal: parse_int(t[0])?,
                justification: parse_int(t[1])?,
                depth: parse_uint(t[2])? as u64,
            });
        }
        let c = l.keyword_count("conflicts", usize::MAX)?;
        let mut conflicts = Vec::with_capacity(c.min(1 << 20));
        for _ in 0..c {
            let t = l.tokens()?;
            if t.len() != 1 {
                return Err(malformed("conflict line must hold one atom"));
            }
            conflicts.push(parse_uint(t[0])? as u64);
        }
        let t = l.tokens()?;
        let consistent = match t.as_slice() {
            ["consistent", "1"] => true,
            ["consistent", "0"] => false,
            _ => return Err(malformed("expected consistent flag")),
        };
        l.expect("end")?;
        l.finish()?;
        Ok(Deduced {
            derived,
            conflicts,
            consistent,
        })
    }
}

/// Which implementation evaluates formal contexts.
#[derive(Debug, Clone)]
pub enum Deductor {
    Rust,
    Mojo(PathBuf),
    /// Run both; disagreement fails closed.
    Cross(PathBuf),
}

impl Deductor {
    pub fn from_spec(spec: &str) -> Result<Deductor> {
        if spec == "rust" {
            return Ok(Deductor::Rust);
        }
        if let Some(p) = spec.strip_prefix("mojo:") {
            return Ok(Deductor::Mojo(PathBuf::from(p)));
        }
        if let Some(p) = spec.strip_prefix("cross:") {
            return Ok(Deductor::Cross(PathBuf::from(p)));
        }
        Err(CoreError::bad_args(format!(
            "unknown deductor spec {spec:?}"
        )))
    }

    pub fn kind(&self) -> &'static str {
        match self {
            Deductor::Rust => "rust",
            Deductor::Mojo(_) => "mojo",
            Deductor::Cross(_) => "cross",
        }
    }

    /// Identity of the checker implementation, recorded in verification receipts.
    pub fn implementation_digest(&self) -> Result<String> {
        let rust = format!("rust-reference/{}/jacobi-v1", env!("CARGO_PKG_VERSION"));
        match self {
            Deductor::Rust => Ok(rust),
            Deductor::Mojo(p) => Ok(format!("mojo/{}", binary_digest(p)?)),
            Deductor::Cross(p) => Ok(format!("cross({rust},mojo/{})", binary_digest(p)?)),
        }
    }

    pub fn run(&self, p: &Problem) -> Result<Deduced> {
        match self {
            Deductor::Rust => Ok(solve(p)),
            Deductor::Mojo(bin) => run_external(bin, p),
            Deductor::Cross(bin) => {
                let reference = solve(p);
                let external = run_external(bin, p)?;
                if reference != external {
                    return Err(CoreError::new(
                        "DEDUCTOR_DISAGREEMENT",
                        "Mojo deductor output differs from the Rust reference",
                    ));
                }
                Ok(reference)
            }
        }
    }
}

fn binary_digest(p: &PathBuf) -> Result<String> {
    let bytes = std::fs::read(p)
        .map_err(|e| CoreError::new("DEDUCTOR_UNAVAILABLE", format!("{}: {e}", p.display())))?;
    Ok(blob_digest(&bytes))
}

fn run_external(bin: &PathBuf, p: &Problem) -> Result<Deduced> {
    let dir = std::env::temp_dir();
    let unique = crate::canonical::blob_digest(p.render().as_bytes());
    let file = dir.join(format!(
        "intellectus-deduce-{}-{}.txt",
        std::process::id(),
        &unique[7..23]
    ));
    std::fs::write(&file, p.render())
        .map_err(|e| CoreError::new("DEDUCTOR_UNAVAILABLE", e.to_string()))?;
    let out = Command::new(bin).arg(&file).output();
    let _ = std::fs::remove_file(&file);
    let out =
        out.map_err(|e| CoreError::new("DEDUCTOR_UNAVAILABLE", format!("{}: {e}", bin.display())))?;
    if !out.status.success() {
        return Err(CoreError::new(
            "DEDUCTOR_ERROR",
            format!(
                "exit {:?}: {}",
                out.status.code(),
                String::from_utf8_lossy(&out.stderr)
            ),
        ));
    }
    let text = String::from_utf8(out.stdout)
        .map_err(|_| CoreError::new("DEDUCTOR_ERROR", "non-UTF-8 output"))?;
    Deduced::parse(&text)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p(atoms: usize, facts: &[i64], rules: &[(i64, &[i64])]) -> Problem {
        Problem {
            atoms,
            facts: facts.to_vec(),
            rules: rules
                .iter()
                .map(|(h, b)| Rule {
                    head: *h,
                    body: b.to_vec(),
                })
                .collect(),
        }
    }

    #[test]
    fn chain_and_min_index() {
        // r0: 3 <- 1,2 ; r1: 3 <- 1 ; r2: 4 <- 3
        let d = solve(&p(4, &[1, 2], &[(3, &[1, 2]), (3, &[1]), (4, &[3])]));
        let got: Vec<_> = d
            .derived
            .iter()
            .map(|x| (x.literal, x.justification, x.depth))
            .collect();
        assert_eq!(got, vec![(1, -1, 0), (2, -1, 0), (3, 0, 1), (4, 2, 2)]);
        assert!(d.consistent);
    }

    #[test]
    fn earliest_round_wins_over_smaller_index() {
        // r0: 3 <- 2 (2 derived in round 1), r1: 3 <- 1 (round 1); r2: 2 <- 1.
        let d = solve(&p(3, &[1], &[(3, &[2]), (3, &[1]), (2, &[1])]));
        let three = d.derived.iter().find(|x| x.literal == 3).unwrap();
        assert_eq!((three.justification, three.depth), (1, 1));
    }

    #[test]
    fn circular_support_is_not_grounded() {
        let d = solve(&p(2, &[], &[(1, &[2]), (2, &[1])]));
        assert!(d.derived.is_empty());
    }

    #[test]
    fn conflict_detected() {
        let d = solve(&p(2, &[1], &[(-2, &[1]), (2, &[])]));
        assert_eq!(d.conflicts, vec![2]);
        assert!(!d.consistent);
    }

    #[test]
    fn roundtrip_text() {
        let prob = p(3, &[1, -2], &[(3, &[1, -2]), (-3, &[])]);
        assert_eq!(Problem::parse(&prob.render()).unwrap(), prob);
        let d = solve(&prob);
        assert_eq!(Deduced::parse(&d.render()).unwrap(), d);
    }

    #[test]
    fn malformed_inputs_rejected() {
        for bad in [
            "",
            "INTELLECTUS-DEDUCE 2\natoms 0\nfacts 0\nrules 0\nend\n",
            "INTELLECTUS-DEDUCE 1\natoms 1\nfacts 1\n0\nrules 0\nend\n",
            "INTELLECTUS-DEDUCE 1\natoms 1\nfacts 1\n2\nrules 0\nend\n",
            "INTELLECTUS-DEDUCE 1\natoms 1\nfacts 0\nrules 1\n1 2 1\nend\n",
            "INTELLECTUS-DEDUCE 1\natoms 1\nfacts 0\nrules 0\nend\nextra\n",
            "INTELLECTUS-DEDUCE 1\natoms 1\nfacts 0\nrules 0\n",
            "INTELLECTUS-DEDUCE 1\natoms  1\nfacts 0\nrules 0\nend\n",
        ] {
            assert!(Problem::parse(bad).is_err(), "accepted {bad:?}");
        }
    }
}
