package api

// Nothing: lease labels arrive only on the owner's routes (POST
// /api/sandboxes create, POST …/comment), which set the comment and the
// events every consumer of this package needs. #119's runner label is
// client-side (runner/lease_adapter.go), and #132's dashboard reads the
// label from the lease row, so no extra seam exists here.
