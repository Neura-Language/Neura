#!/usr/bin/env bash
set -euo pipefail
cd /opt/neura
mkdir -p testdata/pos

cat > testdata/pos/P01.neura <<'EOF'
struct Point { x: Int; y: Int }

fn manhattan(p: Point) -> Int {
  let mut sum = 0
  if p.x < 0 {
    sum = sum - p.x
  } else {
    sum = sum + p.x
  }
  sum
}
EOF

cat > testdata/pos/P02.neura <<'EOF'
logic Echo {
  structure: { n: Int }
  capability: { run() -> Unit {} }
}
instance E1 : Echo { structure: { n: 1 } }

fn go(x: Echo) -> Unit {
  match x against LogicDB {
    case E1(e) => { invoke e.run() }
  }
}
EOF

cat > testdata/pos/P03.neura <<'EOF'
neuron A { output o: Signal<Int> }
neuron B { input i: Signal<Int> }

synapse A.o -> B.i on_failure retry(2) failover failover

logic L {
  structure: { k: Int }
  capability: { run() -> Unit {} }
}
instance I : L { structure: { k: 7 } }

place I at local where gpu_mem >= 1
EOF

cat > testdata/pos/P04.neura <<'EOF'
fn sum_odds(n: Int) -> Int {
  let mut total = 0
  for i in 0..n {
    if i == 0 { continue }
    total = total + i
  }
  for j in 0..=5 {
    if j > 3 { break }
  }
  total
}
EOF

cat > testdata/pos/P05.neura <<'EOF'
fn pair(a: (Int, String)) -> (Int, String) { a }

fn apply(f: fn(Int, Int) -> Int, x: Int) -> Int { f(x, x) }

fn pair_add(a: Int, b: Int) -> Int { a + b }

fn use_apply() -> Int { apply(pair_add, 1) }
EOF

cat > testdata/pos/P06.neura <<'EOF'
logic L6 {
  structure: { user_id: Int; region: String }
  capability: { run() -> Unit {} }
}
instance I6 : L6 { structure: { user_id: 1; region: "eu" } }

place I6 at ["a", "b"] balance consistent_hash(user_id) where user_id >= 1
EOF

cat > testdata/pos/P07.neura <<'EOF'
logic Counter {
  structure: { n: Int }
  capability: {
    bump() -> Unit {
      self.n = 1
    }
  }
}
instance C : Counter { structure: { n: 0 } }

fn pure_max(a: Int, b: Int) -> Int {
  if a > b { a } else { b }
}

fn impure_log(x: Int) -> Int {
  learn(x)
  x
}

place C at local where pure_max(gpu_mem, 1) >= 1
EOF

echo created
