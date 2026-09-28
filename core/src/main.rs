//! `intellectus-core` CLI: init, serve, replay, deduce, keygen, sign.

use std::collections::BTreeMap;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::ExitCode;

use intellectus_core::coordinator::{replay_store, system_clock, Coordinator};
use intellectus_core::deduce::{Deductor, Problem};
use intellectus_core::error::CoreError;
use intellectus_core::policy::{public_key_hex, sign_operator, Policy};
use intellectus_core::server::serve;
use intellectus_core::store::Store;
use serde_json::json;

const USAGE: &str = "usage:
  intellectus-core init --db <file> --project <id> --operator-pubkey <hex> --policy <file> --root-dir <dir>
  intellectus-core serve --db <file> [--deductor rust|mojo:<bin>|cross:<bin>]
  intellectus-core replay --db <file>
  intellectus-core deduce [--impl rust|mojo:<bin>|cross:<bin>] <problem-file>
  intellectus-core keygen --out <file>
  intellectus-core sign --key <file> --body <file>";

fn flag(args: &[String], name: &str) -> Option<String> {
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1))
        .cloned()
}

fn need(args: &[String], name: &str) -> Result<String, CoreError> {
    flag(args, name).ok_or_else(|| CoreError::bad_args(format!("missing {name}\n{USAGE}")))
}

fn io(e: std::io::Error) -> CoreError {
    CoreError::new("IO", e.to_string())
}

fn read_tree(root: &Path) -> Result<BTreeMap<String, Vec<u8>>, CoreError> {
    fn walk(base: &Path, dir: &Path, out: &mut BTreeMap<String, Vec<u8>>) -> Result<(), CoreError> {
        let mut entries: Vec<_> = std::fs::read_dir(dir)
            .map_err(io)?
            .collect::<Result<_, _>>()
            .map_err(io)?;
        entries.sort_by_key(|e| e.path());
        for e in entries {
            let p = e.path();
            let ft = e.file_type().map_err(io)?;
            if ft.is_symlink() {
                return Err(CoreError::bad_args(format!(
                    "symlinks are not supported: {}",
                    p.display()
                )));
            }
            if ft.is_dir() {
                walk(base, &p, out)?;
            } else {
                let rel = p
                    .strip_prefix(base)
                    .expect("under base")
                    .to_string_lossy()
                    .replace('\\', "/");
                out.insert(rel, std::fs::read(&p).map_err(io)?);
            }
        }
        Ok(())
    }
    let mut out = BTreeMap::new();
    walk(root, root, &mut out)?;
    Ok(out)
}

fn read_seed(path: &str) -> Result<[u8; 32], CoreError> {
    let text = std::fs::read_to_string(path).map_err(io)?;
    hex::decode(text.trim())
        .ok()
        .and_then(|b| b.try_into().ok())
        .ok_or_else(|| CoreError::bad_args("key file must hold a 32-byte hex seed"))
}

fn run(args: Vec<String>) -> Result<(), CoreError> {
    let cmd = args.first().map(String::as_str).unwrap_or("");
    let rest = &args[1.min(args.len())..];
    match cmd {
        "init" => {
            let db = PathBuf::from(need(rest, "--db")?);
            let policy: Policy = serde_json::from_str(
                &std::fs::read_to_string(need(rest, "--policy")?).map_err(io)?,
            )
            .map_err(|e| CoreError::bad_args(format!("policy: {e}")))?;
            let files = read_tree(Path::new(&need(rest, "--root-dir")?))?;
            let c = Coordinator::init(
                &db,
                &need(rest, "--project")?,
                vec![need(rest, "--operator-pubkey")?],
                policy,
                files,
                Deductor::Rust,
                system_clock(),
            )?;
            println!(
                "{}",
                json!({"project_id": c.state().project_id, "approved_root": c.state().approved_root, "head_sequence": c.state().sequence})
            );
        }
        "serve" => {
            let deductor =
                Deductor::from_spec(&flag(rest, "--deductor").unwrap_or_else(|| "rust".into()))?;
            deductor.implementation_digest()?; // fail closed if the configured deductor is unavailable
            let mut c =
                Coordinator::open(Path::new(&need(rest, "--db")?), deductor, system_clock())?;
            let stdin = std::io::stdin();
            serve(&mut c, stdin.lock(), std::io::stdout().lock()).map_err(io)?;
        }
        "replay" => {
            let store = Store::open(Path::new(&need(rest, "--db")?))?;
            let (n, d) = replay_store(&store)?;
            println!(
                "{}",
                json!({"events": n, "state_digest": d, "matches": true})
            );
        }
        "deduce" => {
            let spec = flag(rest, "--impl").unwrap_or_else(|| "rust".into());
            let file = rest
                .last()
                .filter(|f| !f.starts_with("--") && Some(*f) != flag(rest, "--impl").as_ref())
                .ok_or_else(|| CoreError::bad_args(USAGE))?;
            let mut text = String::new();
            std::fs::File::open(file)
                .map_err(io)?
                .read_to_string(&mut text)
                .map_err(io)?;
            let problem = Problem::parse(&text)?;
            let out = Deductor::from_spec(&spec)?.run(&problem)?;
            print!("{}", out.render());
        }
        "keygen" => {
            let out = need(rest, "--out")?;
            let mut seed = [0u8; 32];
            std::fs::File::open("/dev/urandom")
                .and_then(|mut f| f.read_exact(&mut seed))
                .map_err(io)?;
            std::fs::write(&out, hex::encode(seed)).map_err(io)?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&out, std::fs::Permissions::from_mode(0o600))
                    .map_err(io)?;
            }
            println!("{}", json!({"public_key": public_key_hex(&seed)}));
        }
        "sign" => {
            let seed = read_seed(&need(rest, "--key")?)?;
            let body = std::fs::read_to_string(need(rest, "--body")?).map_err(io)?;
            println!(
                "{}",
                serde_json::to_string(&sign_operator(&seed, body.trim_end())).expect("serializes")
            );
        }
        _ => return Err(CoreError::bad_args(USAGE)),
    }
    Ok(())
}

fn main() -> ExitCode {
    match run(std::env::args().skip(1).collect()) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!(
                "{}",
                json!({"error": {"code": e.code, "message": e.message}})
            );
            if e.code == "DEDUCE_MALFORMED" {
                ExitCode::from(2)
            } else {
                ExitCode::FAILURE
            }
        }
    }
}
