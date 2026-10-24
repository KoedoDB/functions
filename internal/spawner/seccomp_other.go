//go:build !amd64 && !arm64

package spawner

// trapTable: no table for this architecture; the child refuses to start (it fails closed, it does not run a customer without the tripwire).
func trapTable() (uint32, []uint32, bool) { return 0, nil, false }
