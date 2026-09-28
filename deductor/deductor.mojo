# THE INTELLECTUS -- The Absolute Mathematical Deductor (Mojo).
#
# Usage: deductor <problem-file>
#
# Reads a deduction problem in the text format of docs/PROTOCOL.md section 7,
# computes the Jacobi-round fixpoint of the Horn rules over the facts, and
# prints the result in the section 7 output format on stdout.
#
# Exit codes:
#   0  success (result printed on stdout)
#   1  usage error or the problem file could not be read
#   2  malformed input (nothing printed on stdout; message on stderr)
#
# Internal literal index: literal +i -> 2*(i-1), literal -i -> 2*(i-1)+1.
# Ascending index order is exactly the required output order
# (|lit| ascending, positive before negative).
#
# Algorithm (linear in input size): each rule keeps a counter of body literal
# occurrences not yet derived. The literals derived in round k (the frontier)
# decrement the counters of the rules they occur in; a rule whose counter
# reaches 0 while processing frontier k has its whole body in D_k for the
# first time, so it is exactly one of the rules that fire in round k+1
# (a rule whose body was already in D_{k-1} fired in round k, so its head is
# already in D_k). Among the rules completed in round k, the head's
# justification is the minimum rule index; heads already in D_k are skipped.
# Empty-body rules complete in round 0 and so fire in round 1.

from std.sys import argv, exit, stderr
from std.io import FileDescriptor

comptime MAX_ATOMS = 1_000_000
comptime MAX_RULES = 1_000_000
comptime MAX_BODY_TOTAL = 10_000_000
# Any count above this is certainly malformed (it cannot fit in a real file)
# and bounding it keeps the decimal parser free of overflow.
comptime MAX_NUMBER = 1_000_000_000_000

comptime SP: Int = 32
comptime NL: Int = 10
comptime MINUS: Int = 45
comptime ZERO: Int = 48
comptime NINE: Int = 57


struct Parser:
    var data: List[UInt8]
    var pos: Int
    var line: Int

    def __init__(out self, var data: List[UInt8]):
        self.data = data^
        self.pos = 0
        self.line = 1

    def fail(self, msg: String) raises:
        raise Error("line " + String(self.line) + ": " + msg)

    def peek(self) -> Int:
        if self.pos >= len(self.data):
            return -1
        return Int(self.data[self.pos])

    def expect_text(mut self, text: String) raises:
        """Consume exactly the bytes of `text`."""
        var bytes = text.as_bytes()
        for i in range(len(bytes)):
            if self.peek() != Int(bytes[i]):
                self.fail("expected '" + text + "'")
            self.pos += 1

    def expect_byte(mut self, b: Int, what: String) raises:
        if self.peek() != b:
            self.fail("expected " + what)
        self.pos += 1
        if b == NL:
            self.line += 1

    def uint(mut self) raises -> Int:
        """Canonical non-negative decimal: 0 or [1-9][0-9]*."""
        var c = self.peek()
        if c < ZERO or c > NINE:
            self.fail("expected a decimal number")
        self.pos += 1
        var v = c - ZERO
        if v == 0:
            var d = self.peek()
            if d >= ZERO and d <= NINE:
                self.fail("leading zero in number")
            return 0
        while True:
            var d = self.peek()
            if d < ZERO or d > NINE:
                break
            v = v * 10 + (d - ZERO)
            if v > MAX_NUMBER:
                self.fail("number too large")
            self.pos += 1
        return v

    def literal(mut self, n_atoms: Int) raises -> Int:
        """Parse a literal and return its internal index."""
        var neg = False
        if self.peek() == MINUS:
            neg = True
            self.pos += 1
        var a = self.uint()
        if a == 0:
            self.fail("literal 0 is not allowed")
        if a > n_atoms:
            self.fail("literal |" + String(a) + "| exceeds atoms " + String(n_atoms))
        if neg:
            return 2 * (a - 1) + 1
        return 2 * (a - 1)

    def keyword_count(mut self, keyword: String, limit: Int) raises -> Int:
        """Parse a line `<keyword> <count>\\n`; limit < 0 means unbounded."""
        self.expect_text(keyword)
        self.expect_byte(SP, "a single space")
        var v = self.uint()
        if limit >= 0 and v > limit:
            self.fail(keyword + " " + String(v) + " exceeds limit " + String(limit))
        self.expect_byte(NL, "end of line")
        return v


def append_int(mut out: List[UInt8], value: Int):
    var v = value
    if v < 0:
        out.append(UInt8(MINUS))
        v = -v
    if v == 0:
        out.append(UInt8(ZERO))
        return
    var div = 1
    while div <= v // 10:
        div *= 10
    while div > 0:
        out.append(UInt8(ZERO + (v // div) % 10))
        div //= 10


def append_text(mut out: List[UInt8], text: String):
    for b in text.as_bytes():
        out.append(b)


def index_to_literal(idx: Int) -> Int:
    var atom = idx // 2 + 1
    if idx % 2 == 1:
        return -atom
    return atom


def run(var data: List[UInt8]) raises -> List[UInt8]:
    var p = Parser(data^)

    # ---- header -----------------------------------------------------------
    p.expect_text("INTELLECTUS-DEDUCE 1")
    p.expect_byte(NL, "end of line after header")

    var n_atoms = p.keyword_count("atoms", MAX_ATOMS)
    var n_lits = 2 * n_atoms

    # depth[idx] = -1 if the literal is not derived, else its round.
    var depth = List[Int32](length=n_lits, fill=-1)
    var just = List[Int32](length=n_lits, fill=-1)

    # ---- facts ------------------------------------------------------------
    var n_facts = p.keyword_count("facts", -1)
    var frontier = List[Int32]()
    for _ in range(n_facts):
        var idx = p.literal(n_atoms)
        p.expect_byte(NL, "end of line after fact")
        if depth[idx] < 0:
            depth[idx] = 0
            frontier.append(Int32(idx))

    # ---- rules ------------------------------------------------------------
    var n_rules = p.keyword_count("rules", MAX_RULES)
    var head = List[Int32](length=n_rules, fill=0)
    var remaining = List[Int32](length=n_rules, fill=0)
    var body_start = List[Int32](length=n_rules + 1, fill=0)
    var body = List[Int32]()
    var empty_rules = List[Int32]()
    var total_body = 0
    for r in range(n_rules):
        head[r] = Int32(p.literal(n_atoms))
        p.expect_byte(SP, "a single space")
        var n = p.uint()
        total_body += n
        if total_body > MAX_BODY_TOTAL:
            p.fail("total body literals exceed limit " + String(MAX_BODY_TOTAL))
        body_start[r] = Int32(len(body))
        for _ in range(n):
            p.expect_byte(SP, "a single space")
            body.append(Int32(p.literal(n_atoms)))
        p.expect_byte(NL, "end of line after rule")
        remaining[r] = Int32(n)
        if n == 0:
            empty_rules.append(Int32(r))
    body_start[n_rules] = Int32(len(body))

    # ---- trailer ----------------------------------------------------------
    p.expect_text("end")
    if p.peek() == NL:
        p.pos += 1
    if p.pos != len(p.data):
        p.fail("trailing data after 'end'")

    # ---- occurrence lists (CSR): literal -> rules with it in their body ---
    var occ_start = List[Int32](length=n_lits + 1, fill=0)
    for i in range(len(body)):
        occ_start[Int(body[i]) + 1] += 1
    for i in range(n_lits):
        occ_start[i + 1] += occ_start[i]
    var fill_pos = List[Int32](length=n_lits, fill=0)
    for i in range(n_lits):
        fill_pos[i] = occ_start[i]
    var occ = List[Int32](length=len(body), fill=0)
    for r in range(n_rules):
        for j in range(Int(body_start[r]), Int(body_start[r + 1])):
            var l = Int(body[j])
            occ[Int(fill_pos[l])] = Int32(r)
            fill_pos[l] += 1

    # ---- Jacobi rounds ----------------------------------------------------
    var k = 0
    var next = List[Int32]()
    # Round 0 -> 1: empty-body rules are complete with respect to D_0.
    for i in range(len(empty_rules)):
        var r = Int(empty_rules[i])
        var h = Int(head[r])
        var d = Int(depth[h])
        if d < 0:
            depth[h] = Int32(k + 1)
            just[h] = Int32(r)
            next.append(Int32(h))
        elif d == k + 1 and r < Int(just[h]):
            just[h] = Int32(r)
    while True:
        for fi in range(len(frontier)):
            var l = Int(frontier[fi])
            for oi in range(Int(occ_start[l]), Int(occ_start[l + 1])):
                var r = Int(occ[oi])
                var c = remaining[r] - 1
                remaining[r] = c
                if c == 0:
                    var h = Int(head[r])
                    var d = Int(depth[h])
                    if d < 0:
                        depth[h] = Int32(k + 1)
                        just[h] = Int32(r)
                        next.append(Int32(h))
                    elif d == k + 1 and r < Int(just[h]):
                        just[h] = Int32(r)
        if len(next) == 0:
            break
        frontier = next^
        next = List[Int32]()
        k += 1

    # ---- output -----------------------------------------------------------
    var m = 0
    for i in range(n_lits):
        if depth[i] >= 0:
            m += 1
    var conflicts = List[Int32]()
    for a in range(n_atoms):
        if depth[2 * a] >= 0 and depth[2 * a + 1] >= 0:
            conflicts.append(Int32(a + 1))

    var out = List[UInt8](capacity=64 + m * 24 + len(conflicts) * 8)
    append_text(out, "INTELLECTUS-DEDUCED 1\nderived ")
    append_int(out, m)
    out.append(UInt8(NL))
    for i in range(n_lits):
        var d = Int(depth[i])
        if d >= 0:
            append_int(out, index_to_literal(i))
            out.append(UInt8(SP))
            append_int(out, Int(just[i]))
            out.append(UInt8(SP))
            append_int(out, d)
            out.append(UInt8(NL))
    append_text(out, "conflicts ")
    append_int(out, len(conflicts))
    out.append(UInt8(NL))
    for i in range(len(conflicts)):
        append_int(out, Int(conflicts[i]))
        out.append(UInt8(NL))
    if len(conflicts) == 0:
        append_text(out, "consistent 1\nend\n")
    else:
        append_text(out, "consistent 0\nend\n")
    return out^


def main():
    var args = argv()
    if len(args) != 2:
        print("usage: deductor <problem-file>", file=stderr)
        exit(1)

    var data = List[UInt8]()
    try:
        with open(String(args[1]), "r") as f:
            data = f.read_bytes()
    except e:
        print("deductor: cannot read problem file: " + String(e), file=stderr)
        exit(1)

    var out = List[UInt8]()
    try:
        out = run(data^)
    except e:
        print("deductor: malformed input: " + String(e), file=stderr)
        exit(2)

    var stdout_fd = FileDescriptor(1)
    stdout_fd.write_bytes(Span(out))
