"""Python side of the Semantic Map: the prior initialization pipeline.

`prior_init` reads the published constants from P1–P5 and emits
`prior_weights.json`, the calibration the Go daemon seeds its state model from.
That is the whole of the Python layer, and it is a build-time step rather than a
runtime component — nothing here runs on an edge node.

There used to be more: a mirror of the contract interfaces as ABCs, their
compliance suites, and a `SemanticMap` facade with a profile registry. The idea was
that the Python definitions were the specification and the Go interfaces
implemented them, so passing both suites proved behavioural equivalence across
languages. Nothing was ever built behind the Python side — the `cloud-full` profile
that would have used it does not exist — so what the mirror provided was a second
definition of the contract surface with no implementation to keep it honest. It
drifted accordingly: it still declared Storage and Updater after both were deleted
from Go, and its Ontology carried a strength and an audit log the Go interface had
stopped holding. A specification no implementation is checked against is not a
specification, and a reader had no way to tell which of the two definitions was
current.

The Go interfaces in `go/pkg/contracts` are the contract surface, and
`go/compliance` is what checks it.
"""
