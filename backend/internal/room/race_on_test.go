//go:build race

package room

// raceBuild reports whether the race detector is compiled in. Throughput assertions
// are skipped under -race because the detector slows atomics and map access by an
// order of magnitude, making the numbers meaningless.
const raceBuild = true
