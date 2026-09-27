//! JSON Lines command server over stdin/stdout (PROTOCOL.md §2).

use std::io::{BufRead, Write};

use serde::Deserialize;
use serde_json::{json, Value};

use crate::coordinator::Coordinator;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    id: u64,
    cmd: String,
    #[serde(default)]
    args: Value,
}

/// Serve requests strictly in arrival order until EOF.
pub fn serve<R: BufRead, W: Write>(
    coord: &mut Coordinator,
    input: R,
    mut output: W,
) -> std::io::Result<()> {
    for line in input.lines() {
        let line = line?;
        if line.trim().is_empty() {
            continue;
        }
        let response = match serde_json::from_str::<Request>(&line) {
            Err(e) => {
                json!({"id": null, "ok": false, "error": {"code": "BAD_REQUEST", "message": e.to_string()}})
            }
            Ok(req) => {
                let args = if req.args.is_null() {
                    json!({})
                } else {
                    req.args
                };
                match coord.handle(&req.cmd, args) {
                    Ok(result) => json!({"id": req.id, "ok": true, "result": result}),
                    Err(e) => {
                        json!({"id": req.id, "ok": false, "error": {"code": e.code, "message": e.message}})
                    }
                }
            }
        };
        writeln!(output, "{response}")?;
        output.flush()?;
    }
    Ok(())
}
