//! Rust reference vs. the shared conformance vectors, and (when the Mojo
//! binary is built) randomized differential testing Rust <-> Mojo.

use std::path::{Path, PathBuf};
use std::process::Command;

use intellectus_core::deduce::{solve, Deduced, Deductor, Problem, Rule};

fn vectors_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../conformance/deduce")
}

fn mojo_binary() -> Option<PathBuf> {
    let p = std::env::var("INTELLECTUS_MOJO_DEDUCTOR")
        .map(PathBuf::from)
        .unwrap_or_else(|_| {
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../deductor/build/deductor")
        });
    p.exists().then_some(p)
}

#[test]
fn rust_reference_matches_every_vector() {
    let mut checked = 0;
    for entry in std::fs::read_dir(vectors_dir()).unwrap() {
        let path = entry.unwrap().path();
        if path.extension().and_then(|e| e.to_str()) != Some("in") {
            continue;
        }
        let text = std::fs::read_to_string(&path).unwrap_or_default();
        let name = path.file_name().unwrap().to_string_lossy().to_string();
        let parsed = Problem::parse(&text);
        if name.contains("-bad-") {
            assert!(parsed.is_err(), "{name} must be rejected");
        } else {
            let expected = std::fs::read_to_string(path.with_extension("out")).unwrap();
            assert_eq!(
                solve(&parsed.unwrap_or_else(|e| panic!("{name}: {e}"))).render(),
                expected,
                "{name}"
            );
        }
        checked += 1;
    }
    assert!(checked >= 30, "only {checked} vectors found");
}

#[test]
fn cli_exits_2_on_malformed_input() {
    let bin = env!("CARGO_BIN_EXE_intellectus-core");
    let bad = vectors_dir().join("51-bad-literal-zero.in");
    let out = Command::new(bin)
        .args(["deduce", bad.to_str().unwrap()])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(2));
    assert!(!String::from_utf8_lossy(&out.stdout).contains("INTELLECTUS-DEDUCED"));
}

/// Small deterministic PRNG so the test needs no dependency.
struct Lcg(u64);
impl Lcg {
    fn next(&mut self) -> u64 {
        self.0 = self
            .0
            .wrapping_mul(6364136223846793005)
            .wrapping_add(1442695040888963407);
        self.0 >> 33
    }
    fn below(&mut self, n: u64) -> u64 {
        self.next() % n.max(1)
    }
}

fn random_problem(rng: &mut Lcg) -> Problem {
    let atoms = rng.below(12) as usize + 1;
    let lit = |rng: &mut Lcg| {
        let a = rng.below(atoms as u64) as i64 + 1;
        if rng.below(4) == 0 {
            -a
        } else {
            a
        }
    };
    let facts = (0..rng.below(4)).map(|_| lit(rng)).collect();
    let rules = (0..rng.below(16))
        .map(|_| Rule {
            head: lit(rng),
            body: (0..rng.below(4)).map(|_| lit(rng)).collect(),
        })
        .collect();
    Problem {
        atoms,
        facts,
        rules,
    }
}

#[test]
fn mojo_agrees_with_rust_reference() {
    let Some(bin) = mojo_binary() else {
        eprintln!("SKIPPED: Mojo deductor binary not built (deductor/build.sh)");
        return;
    };
    // Every vector through cross mode.
    let cross = Deductor::Cross(bin.clone());
    for entry in std::fs::read_dir(vectors_dir()).unwrap() {
        let path = entry.unwrap().path();
        let name = path.file_name().unwrap().to_string_lossy().to_string();
        if name.ends_with(".in") && !name.contains("-bad-") {
            let p = Problem::parse(&std::fs::read_to_string(&path).unwrap()).unwrap();
            cross.run(&p).unwrap_or_else(|e| panic!("{name}: {e}"));
        }
    }
    // Randomized differential testing.
    let mut rng = Lcg(0x1A7E_11EC_7005);
    for i in 0..300 {
        let p = random_problem(&mut rng);
        let got: Deduced = cross
            .run(&p)
            .unwrap_or_else(|e| panic!("case {i}: {e}\n{}", p.render()));
        assert_eq!(got, solve(&p));
    }
}
